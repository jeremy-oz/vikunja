# forgejo-mirror

A Vikunja plugin that keeps a one-way copy of Forgejo (or Gitea) issues as tasks.

Forgejo calls a webhook on every issue change; the plugin creates or updates the matching task. Nothing is written back to Forgejo. The rule for people and agents is: edit in Forgejo, read in Vikunja.

It runs inside the stock Vikunja binary through the Yaegi plugin loader, so no custom build is needed. Tested against Vikunja 2.7.0.

## What is mirrored

| In Forgejo | In Vikunja |
|---|---|
| Issue opened | Task created in the mapped project, with a link back to the issue |
| Title or text edited | Task title and description updated |
| Label changed | Task moved to the column mapped to that label |
| Issue closed | Task marked done (Vikunja moves it to the done column) |
| Issue reopened | Task marked not done and placed by its labels again |
| Comment added | Comment added to the task, prefixed with the Forgejo author |

Not mirrored: pull requests, edited or deleted comments, assignees, milestones, attachments, and labels other than the ones mapped to columns.

Issue text and comments are Markdown in Forgejo. With an `api_token` in the config they are rendered by Vikunja's own converter (headings, lists, checkboxes, code blocks, tables). Without one they are copied as plain paragraphs.

Existing issues appear in Vikunja the first time something changes on them. There is no backfill yet.

## Setup

1. Enable plugins in Vikunja's `config.yml` and copy this directory into the plugin directory:

   ```yaml
   plugins:
     enabled: true
     dir: "/var/lib/vikunja/plugins"
     loader: "yaegi"
   ```

   The result is `/var/lib/vikunja/plugins/forgejo-mirror/main.go`.

2. Create `forgejo-mirror.json` in Vikunja's root path (or point `VIKUNJA_FORGEJO_MIRROR_CONFIG` at it). Keep it readable only by the Vikunja user; it holds the webhook secret.

   ```json
   {
     "secret": "a long random string",
     "user": "bot-forgejo",
     "repos": {
       "goku/workspace": { "project_id": 7 }
     },
     "label_columns": {
       "status/backlog": "Backlog",
       "status/ready": "Ready",
       "status/doing": "Doing",
       "status/review": "Review"
     },
     "default_column": "Backlog",
     "api_token": "tk_..."
   }
   ```

   - `user` is the Vikunja account the mirror acts as. Tasks and comments show it as their author.
   - `repos` maps `owner/name` to the target project id. Events from other repositories are ignored.
   - `label_columns` maps a label to the title of a column in the project's kanban view. An open issue with none of these labels goes to `default_column`.
   - `api_token` (optional) is an API token of `user` with `tasks.read_one`, `tasks.update` and `tasks_comments.create`. It switches on Markdown rendering: the plugin hands the text to Vikunja's own API (`?format=markdown`) on the address in `service.interface`. Set `api_url` if that address is not reachable from the server itself.

3. Restart Vikunja. The log shows `Loaded plugin forgejo-mirror` and `GET /api/v1/plugins/forgejo-mirror/status` lists the mapped repositories.

4. In Forgejo, add a webhook of type **Forgejo** (or Gitea) to the repository:

   - Target URL: `https://<your-vikunja>/api/v1/plugins/forgejo-mirror/webhook`
   - HTTP method `POST`, content type `application/json`
   - Secret: the same string as in the config file
   - Trigger on: Issues, Issue labelled, Issue comment

## How it works

- Every delivery is checked against the secret (HMAC-SHA256 of the body, from `X-Forgejo-Signature`, `X-Gitea-Signature` or `X-Hub-Signature-256`). Anything else gets `401`.
- Each issue event carries the whole issue, so one "bring the task in line" step serves all event types and a redelivered event changes nothing.
- A small table, `plugin_forgejo_mirror`, records which issue became which task and which comment became which comment. That is what prevents duplicates. It also keeps a hash of the last rendered text, so the description is only rewritten when the issue text changed.
- Rendering happens after the task change is saved. If it fails, the task keeps its plain text and the log says why; the next event for that issue tries again.
- If a mirrored task is deleted in Vikunja, the next event for that issue creates it again.

## Limits

- One direction only. A change made to a mirrored task in Vikunja is overwritten by the next event from Forgejo.
- A delivery that arrives while Vikunja is down is lost unless Forgejo redelivers it. The next change to the same issue brings the task up to date.
- Vikunja must be reachable from the Forgejo server.
- The plugin uses Vikunja's internal Go packages, which can change between releases. Re-run the checks after an upgrade.
