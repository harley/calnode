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

## Google Calendar

The Google Cloud project is `coderpush-bookings`, owned by `coderpush.com`.
Calendar API is enabled and the OAuth audience is Internal. The web client uses
only `https://book.coderpush.com/v1/auth/callback` and
`https://book.coderpush.com/v1/calendar/callback`. Its credentials are stored in
Calnode's encrypted Google settings and the protected local configuration.

Calendar consent requests event management, free/busy, read-only calendar lists
and read-only calendar metadata. It does not request calendar sharing or deletion.
These scopes cover the provider's calls: [event management](https://developers.google.com/workspace/calendar/api/v3/reference/events/insert),
[availability](https://developers.google.com/workspace/calendar/api/v3/reference/freebusy/query),
[calendar selection](https://developers.google.com/workspace/calendar/api/v3/reference/calendarList/list),
and [account metadata](https://developers.google.com/workspace/calendar/api/v3/reference/calendars/get).
Google sign-in is separate from Calendar consent. Each host must connect their
own account and select the calendars to check and the booking destination.

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

This fork is based on v0.10.1. Do not switch the service to the upstream `edge`
image or auto-merge a later upstream main. CoderPush migrations 68, 69, and 70
have already run in production and keep their original versions. The upstream
invite-delivery and RSVP migrations, originally numbered 68 and 69, are included
as 71 and 72 so they run after the CoderPush history. This migration sequence is
for the CoderPush database lineage, not an upstream database that already ran
upstream versions 68 and 69. Test future upgrades against a restored database.

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

## Google Workspace self-service

Enable self-service in Settings → Google OAuth and allow the exact `coderpush.com`
domain. The same policy is available through GET/PATCH `/v1/settings/google` as
`signup_enabled` and `signup_domains`. Omitted credential fields are preserved.
The default policy is disabled; no company or employee identities are hardcoded.

Before enabling signup after this upgrade, sign in as the known owner once to bind
its Google subject. Existing unbound users retain the old email-based trust for
that first binding; the application cannot distinguish a recycled email at that
point. Later logins bind to the signed stable Google subject and reject conflicts.

Staff open `/admin/login` and use their company Google account. They join as Members
without teams, API keys, working hours, or Calendar access. Confirm the timezone in
Profile, connect Calendar with separate consent, set Availability, then create an
event type. Sales remains inactive until its chosen hosts are ready.

Disabling signup stops new accounts only. Archive a user to revoke existing local
access. Workspace suspension alone does not revoke existing app sessions; local
sessions can last 30 days. Google Directory synchronization is not implemented.
