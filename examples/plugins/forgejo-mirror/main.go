// Vikunja is a to-do list application to facilitate your life.
// Copyright 2018-present Vikunja and contributors. All rights reserved.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

// forgejo-mirror keeps a one-way copy of Forgejo (or Gitea) issues in Vikunja.
// Forgejo calls the webhook route on every issue change; the plugin creates or
// updates the matching task. Nothing is ever written back to Forgejo.
// See README.md in this directory for setup.
//
// Yaegi evaluates this at runtime via its exported factories, so there is no func main
// and it must stay out of `go build ./...`.
//go:build ignore

package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"code.vikunja.io/api/pkg/config"
	"code.vikunja.io/api/pkg/db"
	"code.vikunja.io/api/pkg/log"
	"code.vikunja.io/api/pkg/models"
	"code.vikunja.io/api/pkg/plugins"
	"code.vikunja.io/api/pkg/user"

	"github.com/labstack/echo/v5"
	"src.techknowlogick.com/xormigrate"
	"xorm.io/xorm"
)

const (
	linkTable    = "plugin_forgejo_mirror"
	kindIssue    = "issue"
	kindComment  = "comment"
	maxBodyBytes = 2 << 20
)

// repoConfig says where the issues of one repository go.
type repoConfig struct {
	ProjectID int64 `json:"project_id"`
}

// mirrorConfig is read from forgejo-mirror.json in Vikunja's root path, or from
// the file named by VIKUNJA_FORGEJO_MIRROR_CONFIG.
type mirrorConfig struct {
	// Secret is the webhook secret configured in Forgejo. Required.
	Secret string `json:"secret"`
	// User is the Vikunja username the mirror acts as. Required.
	User string `json:"user"`
	// Repos maps "owner/name" to the target project.
	Repos map[string]repoConfig `json:"repos"`
	// LabelColumns maps a Forgejo label name to a kanban column title.
	LabelColumns map[string]string `json:"label_columns"`
	// DefaultColumn is used for open issues carrying none of the mapped labels.
	DefaultColumn string `json:"default_column"`
	// Label is the title of the label put on every mirrored task so they stand
	// out from tasks created in Vikunja. Defaults to "forgejo"; "none" disables it.
	Label string `json:"label"`
	// LabelColor is the hex colour used when the label has to be created.
	LabelColor string `json:"label_color"`
	// APIToken is an API token of User with tasks.read_one, tasks.update and
	// tasks_comments.create. With it, issue text and comments are rendered from
	// Markdown by Vikunja itself; without it they are copied as plain paragraphs.
	APIToken string `json:"api_token"`
	// APIURL is Vikunja's own v2 API as seen from the server, by default
	// http://<service.interface>/api/v2.
	APIURL string `json:"api_url"`
}

// mirrorLink remembers which Forgejo item became which Vikunja item.
// Interpreted types reach xorm without methods, so the table name is always
// passed via Table() and the column names are spelled out.
type mirrorLink struct {
	ID        int64  `xorm:"'id' pk autoincr"`
	Repo      string `xorm:"'repo' varchar(250) notnull index"`
	Kind      string `xorm:"'kind' varchar(20) notnull"`
	ExtID     int64  `xorm:"'ext_id' bigint notnull index"`
	TaskID    int64  `xorm:"'task_id' bigint notnull"`
	CommentID int64  `xorm:"'comment_id' bigint notnull default 0"`
	// BodyHash is the hash of the Markdown last rendered into the task.
	BodyHash string `xorm:"'body_hash' varchar(64) notnull default ''"`
}

type forgejoUser struct {
	Login string `json:"login"`
}

type forgejoLabel struct {
	Name string `json:"name"`
}

type forgejoIssue struct {
	Number      int64           `json:"number"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	State       string          `json:"state"`
	HTMLURL     string          `json:"html_url"`
	User        forgejoUser     `json:"user"`
	Labels      []forgejoLabel  `json:"labels"`
	PullRequest json.RawMessage `json:"pull_request"`
}

type forgejoComment struct {
	ID      int64       `json:"id"`
	Body    string      `json:"body"`
	HTMLURL string      `json:"html_url"`
	User    forgejoUser `json:"user"`
}

type forgejoRepo struct {
	FullName string `json:"full_name"`
}

type forgejoPayload struct {
	Action     string          `json:"action"`
	Issue      *forgejoIssue   `json:"issue"`
	Comment    *forgejoComment `json:"comment"`
	Repository forgejoRepo     `json:"repository"`
}

type ForgejoMirrorPlugin struct {
	cfg   *mirrorConfig
	mutex sync.Mutex
}

func (p *ForgejoMirrorPlugin) Name() string    { return "forgejo-mirror" }
func (p *ForgejoMirrorPlugin) Version() string { return "0.1.0" }
func (p *ForgejoMirrorPlugin) Shutdown() error { return nil }

func (p *ForgejoMirrorPlugin) Init() error {
	path := os.Getenv("VIKUNJA_FORGEJO_MIRROR_CONFIG")
	if path == "" {
		path = filepath.Join(config.ServiceRootpath.GetString(), "forgejo-mirror.json")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		log.Warningf("forgejo-mirror: no config at %s, the webhook stays disabled: %s", path, err)
		return nil
	}

	cfg := &mirrorConfig{}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return fmt.Errorf("forgejo-mirror: reading %s: %w", path, err)
	}
	if cfg.Secret == "" || cfg.User == "" {
		return fmt.Errorf("forgejo-mirror: %s needs both \"secret\" and \"user\"", path)
	}
	if cfg.DefaultColumn == "" {
		cfg.DefaultColumn = "Backlog"
	}
	if cfg.Label == "" {
		cfg.Label = "forgejo"
	}
	if cfg.LabelColor == "" {
		cfg.LabelColor = "d9480f"
	}
	if cfg.APIURL == "" {
		iface := config.ServiceInterface.GetString()
		if strings.HasPrefix(iface, ":") {
			iface = "127.0.0.1" + iface
		}
		cfg.APIURL = "http://" + iface + "/api/v2"
	}
	cfg.APIURL = strings.TrimRight(cfg.APIURL, "/")

	p.cfg = cfg
	log.Infof("forgejo-mirror: mirroring %d repositories as user %s (markdown rendering: %t)", len(cfg.Repos), cfg.User, cfg.APIToken != "")
	return nil
}

func (p *ForgejoMirrorPlugin) Migrations() []*xormigrate.Migration {
	return []*xormigrate.Migration{
		{
			ID:          "20261010000000-create-plugin-forgejo-mirror",
			Description: "Create the table linking Forgejo issues and comments to tasks",
			Migrate: func(tx *xorm.Engine) error {
				return tx.Table(linkTable).Sync2(&mirrorLink{})
			},
			Rollback: func(tx *xorm.Engine) error {
				return tx.DropTables(linkTable)
			},
		},
		{
			ID:          "20261010010000-plugin-forgejo-mirror-body-hash",
			Description: "Add the body hash column to the Forgejo mirror link table",
			Migrate: func(tx *xorm.Engine) error {
				return tx.Table(linkTable).Sync2(&mirrorLink{})
			},
			Rollback: func(tx *xorm.Engine) error {
				return nil
			},
		},
	}
}

func (p *ForgejoMirrorPlugin) RegisterUnauthenticatedRoutes(g *echo.Group) {
	g.POST("/forgejo-mirror/webhook", handleWebhook)
	g.GET("/forgejo-mirror/status", handleStatus)
}

func handleStatus(c *echo.Context) error {
	repos := []string{}
	if singleton.cfg != nil {
		for name := range singleton.cfg.Repos {
			repos = append(repos, name)
		}
		sort.Strings(repos)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"plugin":     singleton.Name(),
		"version":    singleton.Version(),
		"configured": singleton.cfg != nil,
		"repos":      repos,
	})
}

func handleWebhook(c *echo.Context) error {
	cfg := singleton.cfg
	if cfg == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "forgejo-mirror is not configured")
	}

	req := c.Request()
	body, err := io.ReadAll(io.LimitReader(req.Body, maxBodyBytes))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "could not read the request body")
	}

	if !validSignature(cfg.Secret, body, signatureFrom(req)) {
		return echo.NewHTTPError(http.StatusUnauthorized, "invalid signature")
	}

	event := firstHeader(req, "X-Forgejo-Event", "X-Gitea-Event", "X-GitHub-Event")
	if event != "issues" && event != "issue_comment" {
		return c.JSON(http.StatusOK, map[string]string{"result": "ignored", "reason": "event " + event})
	}

	payload := &forgejoPayload{}
	if err := json.Unmarshal(body, payload); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "payload is not valid JSON")
	}
	if payload.Issue == nil {
		return c.JSON(http.StatusOK, map[string]string{"result": "ignored", "reason": "no issue in payload"})
	}
	if len(payload.Issue.PullRequest) > 0 && string(payload.Issue.PullRequest) != "null" {
		return c.JSON(http.StatusOK, map[string]string{"result": "ignored", "reason": "pull request"})
	}

	repo, mapped := cfg.Repos[payload.Repository.FullName]
	if !mapped {
		return c.JSON(http.StatusOK, map[string]string{"result": "ignored", "reason": "repository not mapped"})
	}

	// One delivery at a time: Forgejo sends bursts (label change plus edit) and
	// two of them creating the same task would leave duplicates.
	singleton.mutex.Lock()
	defer singleton.mutex.Unlock()

	s := db.NewSession()
	defer s.Close()

	taskID, result, pending, err := mirror(s, cfg, repo, event, payload)
	if err != nil {
		_ = s.Rollback()
		log.Errorf("forgejo-mirror: %s#%d: %s", payload.Repository.FullName, payload.Issue.Number, err)
		return echo.NewHTTPError(http.StatusInternalServerError, "mirroring failed, see the Vikunja log")
	}
	if err := s.Commit(); err != nil {
		log.Errorf("forgejo-mirror: commit failed: %s", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "mirroring failed, see the Vikunja log")
	}

	// Rich text goes through Vikunja's own API so its Markdown converter does the
	// rendering. This happens after the commit: the API call needs the database.
	if cfg.APIToken != "" {
		renderPending(cfg, payload.Repository.FullName, payload.Issue.Number, taskID, pending)
	}

	return c.JSON(http.StatusOK, map[string]interface{}{"result": result, "task_id": taskID})
}

// pendingText is the Markdown still to be rendered once the transaction is done.
type pendingText struct {
	description     string
	descriptionHash string
	comment         string
	commentID       int64
}

func renderPending(cfg *mirrorConfig, repoName string, issueNumber, taskID int64, pending *pendingText) {
	if pending == nil {
		return
	}

	if pending.description != "" {
		if err := putDescription(cfg, taskID, pending.description); err != nil {
			log.Errorf("forgejo-mirror: %s#%d: rendering the description: %s", repoName, issueNumber, err)
		} else {
			s := db.NewSession()
			_, err := s.Table(linkTable).
				Where("repo = ? AND kind = ? AND ext_id = ?", repoName, kindIssue, issueNumber).
				Cols("body_hash").
				Update(&mirrorLink{BodyHash: pending.descriptionHash})
			if err == nil {
				err = s.Commit()
			}
			if err != nil {
				log.Errorf("forgejo-mirror: %s#%d: saving the body hash: %s", repoName, issueNumber, err)
			}
			s.Close()
		}
	}

	if pending.comment != "" {
		commentID, err := postComment(cfg, taskID, pending.comment)
		if err != nil {
			log.Errorf("forgejo-mirror: %s#%d: creating the comment: %s", repoName, issueNumber, err)
			return
		}
		s := db.NewSession()
		newLink := &mirrorLink{Repo: repoName, Kind: kindComment, ExtID: pending.commentID, TaskID: taskID, CommentID: commentID}
		_, err = s.Table(linkTable).Insert(newLink)
		if err == nil {
			err = s.Commit()
		}
		if err != nil {
			log.Errorf("forgejo-mirror: %s#%d: saving the comment link: %s", repoName, issueNumber, err)
		}
		s.Close()
	}
}

var apiClient = &http.Client{Timeout: 20 * time.Second}

func apiCall(cfg *mirrorConfig, method, path string, body interface{}) (map[string]interface{}, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, cfg.APIURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := apiClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s answered %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	result := map[string]interface{}{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// putDescription replaces only the description. PUT takes the whole task, so
// the current one is read first and sent back with the new text.
func putDescription(cfg *mirrorConfig, taskID int64, markdown string) error {
	path := fmt.Sprintf("/tasks/%d", taskID)
	task, err := apiCall(cfg, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	task["description"] = markdown
	_, err = apiCall(cfg, http.MethodPut, path+"?format=markdown", task)
	return err
}

func postComment(cfg *mirrorConfig, taskID int64, markdown string) (int64, error) {
	created, err := apiCall(cfg, http.MethodPost, fmt.Sprintf("/tasks/%d/comments?format=markdown", taskID), map[string]string{"comment": markdown})
	if err != nil {
		return 0, err
	}
	id, _ := created["id"].(float64)
	return int64(id), nil
}

// mirror brings the task for the issue in line with the payload. Every issue
// event carries the full issue, so the same upsert serves opened, edited,
// closed, reopened and label changes alike.
func mirror(s *xorm.Session, cfg *mirrorConfig, repo repoConfig, event string, payload *forgejoPayload) (taskID int64, result string, pending *pendingText, err error) {
	doer, err := user.GetUserByUsername(s, cfg.User)
	if err != nil {
		return 0, "", nil, fmt.Errorf("looking up user %s: %w", cfg.User, err)
	}

	issue := payload.Issue
	repoName := payload.Repository.FullName
	closed := issue.State == "closed"
	rich := cfg.APIToken != ""
	pending = &pendingText{}

	link := &mirrorLink{}
	found, err := s.Table(linkTable).
		Where("repo = ? AND kind = ? AND ext_id = ?", repoName, kindIssue, issue.Number).
		Get(link)
	if err != nil {
		return 0, "", nil, err
	}

	task := &models.Task{}
	if found {
		existing, err := models.GetTaskByIDSimple(s, link.TaskID)
		switch {
		case err == nil:
			task = &existing
		case models.IsErrTaskDoesNotExist(err):
			// The task was deleted in Vikunja; forget the link and start over.
			if _, err := s.Table(linkTable).Where("id = ?", link.ID).Delete(&mirrorLink{}); err != nil {
				return 0, "", nil, err
			}
			found = false
		default:
			return 0, "", nil, err
		}
	}

	title := strings.TrimSpace(issue.Title)
	if title == "" {
		title = fmt.Sprintf("%s#%d", repoName, issue.Number)
	}
	plain := describe(repoName, issue)
	markdown := describeMarkdown(repoName, issue)
	hash := hashOf(markdown)

	if !found {
		task = &models.Task{
			Title:       title,
			Description: plain,
			ProjectID:   repo.ProjectID,
			Done:        closed,
		}
		if err := task.Create(s, doer); err != nil {
			return 0, "", nil, fmt.Errorf("creating the task: %w", err)
		}
		newLink := &mirrorLink{Repo: repoName, Kind: kindIssue, ExtID: issue.Number, TaskID: task.ID}
		if _, err := s.Table(linkTable).Insert(newLink); err != nil {
			return 0, "", nil, err
		}
		if rich {
			pending.description = markdown
			pending.descriptionHash = hash
		}
		result = "created"
	} else {
		result = "unchanged"
		changed := task.Title != title || task.Done != closed
		if rich {
			if link.BodyHash != hash {
				pending.description = markdown
				pending.descriptionHash = hash
				result = "updated"
			}
		} else if task.Description != plain {
			task.Description = plain
			changed = true
		}
		if changed {
			task.Title = title
			task.Done = closed
			if err := task.Update(s, doer); err != nil {
				return 0, "", nil, fmt.Errorf("updating the task: %w", err)
			}
			result = "updated"
		}
	}

	if cfg.Label != "none" {
		if err := ensureLabel(s, doer, task.ID, cfg.Label, cfg.LabelColor); err != nil {
			return 0, "", nil, err
		}
	}

	// Closed issues are left where Vikunja's done handling put them.
	if !closed {
		moved, err := placeInColumn(s, doer, task.ID, repo.ProjectID, columnFor(cfg, issue))
		if err != nil {
			return 0, "", nil, err
		}
		if moved && result == "unchanged" {
			result = "moved"
		}
	}

	if event == "issue_comment" && payload.Comment != nil && payload.Action == "created" {
		exists, err := s.Table(linkTable).
			Where("repo = ? AND kind = ? AND ext_id = ?", repoName, kindComment, payload.Comment.ID).
			Exist()
		if err != nil {
			return 0, "", nil, err
		}
		if !exists {
			if rich {
				pending.comment = "**" + payload.Comment.User.Login + "** on Forgejo:\n\n" + payload.Comment.Body
				pending.commentID = payload.Comment.ID
			} else if err := mirrorComment(s, doer, repoName, task.ID, payload.Comment); err != nil {
				return 0, "", nil, err
			}
			result = "commented"
		}
	}

	return task.ID, result, pending, nil
}

func mirrorComment(s *xorm.Session, doer *user.User, repoName string, taskID int64, comment *forgejoComment) error {
	text := "<p><strong>" + html.EscapeString(comment.User.Login) + "</strong> on Forgejo:</p>" + toHTML(comment.Body)
	tc := &models.TaskComment{Comment: text, TaskID: taskID}
	if err := tc.Create(s, doer); err != nil {
		return fmt.Errorf("creating the comment: %w", err)
	}

	newLink := &mirrorLink{Repo: repoName, Kind: kindComment, ExtID: comment.ID, TaskID: taskID, CommentID: tc.ID}
	_, err := s.Table(linkTable).Insert(newLink)
	return err
}

// ensureLabel makes sure the task carries the mirror's label, creating the label
// on first use. The label belongs to the mirror's user.
func ensureLabel(s *xorm.Session, doer *user.User, taskID int64, title, color string) error {
	label := &models.Label{}
	found, err := s.Where("title = ? AND created_by_id = ?", title, doer.ID).Get(label)
	if err != nil {
		return err
	}
	if !found {
		label = &models.Label{Title: title, HexColor: color, Description: "Mirrored from Forgejo. Edit the issue there."}
		if err := label.Create(s, doer); err != nil {
			return fmt.Errorf("creating the label %q: %w", title, err)
		}
	}

	attached, err := s.Where("label_id = ? AND task_id = ?", label.ID, taskID).Exist(&models.LabelTask{})
	if err != nil || attached {
		return err
	}
	lt := &models.LabelTask{LabelID: label.ID, TaskID: taskID}
	if err := lt.Create(s, doer); err != nil {
		return fmt.Errorf("labelling the task: %w", err)
	}
	return nil
}

// columnFor picks the kanban column for an open issue from its labels.
func columnFor(cfg *mirrorConfig, issue *forgejoIssue) string {
	for _, label := range issue.Labels {
		if column, ok := cfg.LabelColumns[label.Name]; ok {
			return column
		}
	}
	return cfg.DefaultColumn
}

// placeInColumn moves the task into the named column of the project's kanban
// view. A project without a kanban view or without that column is left alone.
func placeInColumn(s *xorm.Session, doer *user.User, taskID, projectID int64, column string) (moved bool, err error) {
	view := &models.ProjectView{}
	found, err := s.Where("project_id = ? AND view_kind = ?", projectID, models.ProjectViewKindKanban).
		OrderBy("position asc").
		Get(view)
	if err != nil || !found {
		return false, err
	}

	buckets := []*models.Bucket{}
	if err := s.Where("project_view_id = ?", view.ID).Find(&buckets); err != nil {
		return false, err
	}
	var target *models.Bucket
	for _, bucket := range buckets {
		if strings.EqualFold(bucket.Title, column) {
			target = bucket
			break
		}
	}
	if target == nil {
		log.Warningf("forgejo-mirror: project %d has no column %q", projectID, column)
		return false, nil
	}

	current := &models.TaskBucket{}
	has, err := s.Where("task_id = ? AND project_view_id = ?", taskID, view.ID).Get(current)
	if err != nil {
		return false, err
	}
	if has && current.BucketID == target.ID {
		return false, nil
	}

	tb := &models.TaskBucket{
		BucketID:      target.ID,
		TaskID:        taskID,
		ProjectViewID: view.ID,
		ProjectID:     projectID,
	}
	if err := tb.Update(s, doer); err != nil {
		return false, fmt.Errorf("moving the task to %q: %w", column, err)
	}
	return true, nil
}

func describe(repoName string, issue *forgejoIssue) string {
	ref := html.EscapeString(fmt.Sprintf("%s#%d", repoName, issue.Number))
	header := "<p>Mirrored from <a href=\"" + html.EscapeString(issue.HTMLURL) + "\">" + ref + "</a>"
	if issue.User.Login != "" {
		header += ", opened by " + html.EscapeString(issue.User.Login)
	}
	header += ". Edit it there: changes made here are overwritten.</p>"
	return header + toHTML(issue.Body)
}

// describeMarkdown is the description as Markdown, for Vikunja to render.
func describeMarkdown(repoName string, issue *forgejoIssue) string {
	header := fmt.Sprintf("Mirrored from [%s#%d](%s)", repoName, issue.Number, issue.HTMLURL)
	if issue.User.Login != "" {
		header += ", opened by " + issue.User.Login
	}
	header += ". Edit it there: changes made here are overwritten."
	body := strings.TrimSpace(strings.ReplaceAll(issue.Body, "\r\n", "\n"))
	if body == "" {
		return header
	}
	return header + "\n\n---\n\n" + body
}

func hashOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// toHTML turns Forgejo's Markdown source into plain, safe paragraphs. It does
// not render Markdown; it is the fallback when no API token is configured.
func toHTML(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if text == "" {
		return ""
	}
	out := ""
	for _, paragraph := range strings.Split(text, "\n\n") {
		paragraph = strings.TrimSpace(paragraph)
		if paragraph == "" {
			continue
		}
		out += "<p>" + strings.ReplaceAll(html.EscapeString(paragraph), "\n", "<br>") + "</p>"
	}
	return out
}

func signatureFrom(req *http.Request) string {
	signature := firstHeader(req, "X-Forgejo-Signature", "X-Gitea-Signature", "X-Hub-Signature-256")
	return strings.TrimPrefix(signature, "sha256=")
}

func firstHeader(req *http.Request, names ...string) string {
	for _, name := range names {
		if value := req.Header.Get(name); value != "" {
			return value
		}
	}
	return ""
}

func validSignature(secret string, body []byte, signature string) bool {
	given, err := hex.DecodeString(signature)
	if err != nil || len(given) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), given)
}

// singleton ensures all factory functions return the same instance so that the
// config loaded in Init() is available to the route handlers.
var singleton = &ForgejoMirrorPlugin{}

func NewPlugin() plugins.Plugin { return singleton }

// Typed factory functions for Yaegi compatibility, see the example plugin.
func NewUnauthenticatedRouterPlugin() plugins.UnauthenticatedRouterPlugin { return singleton }
func NewMigrationPlugin() plugins.MigrationPlugin                         { return singleton }
