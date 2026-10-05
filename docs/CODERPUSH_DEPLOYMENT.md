# CoderPush deployment

Deploy this maintained fork to https://book.coderpush.com. The starting release
is Calnode v0.10.1; the CoderPush addition is an editable email-domain policy.

## Runtime

- Railway project/service: `calnode`, production, Singapore, one replica.
- Build with the root Dockerfile. Set `VERSION` to identify the release. GitHub
  deployments stamp Railway's commit SHA automatically; set `COMMIT` for CLI builds.
- `BASE_URL=https://book.coderpush.com`, `PORT=8080`.
- Mount the persistent volume at `/data`. Set `DATABASE_URL=sqlite:///data/calnode.db`
  and `DATA_DIR=/data` so the database and uploaded branding survive deployment.
- Preserve the sealed `CALNODE_ENCRYPTION_KEY` and `CALNODE_RECOVERY_SECRET`.
  Email and calendar credentials in the database require the original encryption key.
- Cloudflare: DNS-only CNAME `book` to the Railway-assigned target, plus Railway's
  verification TXT. Railway terminates HTTPS.
- Enable Railway daily and weekly volume backups. Take a manual backup before upgrades.

Keep credentials outside the repository. The local owner/deployment credentials
are stored with mode 0600 at `~/.config/calnode/coderpush/secrets.json`.
Resend sending credentials are configured in Calnode's encrypted email settings.

## Booking configuration

Create team members through invitations, then add them to the Sales team and the
event's host rotation. Hosts and their addresses are configuration, not source code.
Use round robin with equal priorities to balance bookings among hosts who are free.
Connect each member's Google Calendar and confirm their working hours before
activating the public Sales event.

Set `blocked_email_domains` to `["gmail.com", "hotmail.com"]` on the Sales event.
Edit it in Event Types > Scheduling > Blocked email domains or through the event-type
API. An empty list allows all domains. Names are normalized, duplicates removed,
and matching subdomains are blocked. Booking API, embeds and agent booking paths
enforce the policy on the server. Duplicated events inherit it.

## Upgrade and recovery

This fork is pinned to v0.10.1. Do not switch the service to the upstream `edge`
image or auto-merge upstream main. The custom migration uses version 68; upstream
main already has a different version 68. Before an upgrade, reconcile the migration
history and test the upgrade against a restored copy of the database. Renaming a
migration after it has run does not repair that history.

For recovery, stop new bookings, preserve the current volume, restore a known-good
Railway backup, and deploy its matching code revision with the original encryption
key. Rolling code back to the upstream image alone will not undo the custom schema.
Check `/readyz`, the owner login, event configuration, and a controlled booking
after recovery. A completed snapshot is not proof of a successful restore drill.

## Validation

Run `go test ./...`, `go vet ./...`, and `pnpm check && pnpm build` in `frontend`.
Policy tests cover normalization, quoted local parts, subdomains, rejection without
persisting a booking, editable configuration, creation, copying, and clearing it.
After deployment, verify the release through `/version` and test the public booking
flow using a clearly labelled test event, then cancel the test booking.
