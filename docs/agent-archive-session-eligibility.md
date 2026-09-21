# Session start eligibility

A session is eligible only after its project has been included. Codex and Claude Code must provide `source: startup` or `source: clear` on a new `SessionStart`. Missing, unknown, resume, and compact sources do not establish the start of a previously unseen session.

Unknown Cursor sessions remain uncollected until the adapter has verified start provenance. The current official hook documentation describes `sessionStart` as creation of a new composer conversation, but this implementation has not live-verified resume behavior for a supported Cursor version. A version string alone is not proof. This is a capture limitation, not a claim of complete Cursor support.

Already accepted registrations retain their original start time on resume. An absent transcript path does not erase the saved path. A different harness or project cannot replace the registration identity.

For included projects, skipped starts produce a bounded local diagnostic with application, project, reason, and time. Diagnostic records contain no transcript, native session ID, or transcript path. Excluded project paths are not recorded. `status` and `status --json` show these reasons; hooks remain local and return successfully to the agent.
