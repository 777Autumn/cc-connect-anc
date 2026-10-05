# ANC upstream fixes: post attachments and failed sessions

This change adapts two upstream fixes to the existing protected ANC paths:

- [#1888](https://github.com/chenhg5/cc-connect/pull/1888), main commit
  `5f086d90c98e55f814101da3a3f652a139fcd72d`: recognize rich-text `files[]`.
  ANC validates every locale before download and uses its existing bounded,
  message-owned receiver. A failed resource rejects the complete batch rather
  than silently skipping the attachment. Unknown-parent quote rules stay intact.
- [#1901](https://github.com/chenhg5/cc-connect/pull/1901), main commit
  `4127c3d330515b04e59042597bab1a482f549bc0`: preserve a native session ID
  acquired before an error. ANC records it on the failed turn's Session with the
  actual reply agent, preserving native provenance and permanent expiry markers.

No new model interface, dependency, configuration or state migration is needed.
Upstream busy-lock recovery (#1838) is deferred, not included in this release.

Regression coverage is in `platform/feishu/file_post_test.go` and
`core/error_session_test.go`; existing file, connection and shared Codex journeys
remain release gates. Real `post.files[]` acceptance must be recorded separately
from synthetic tests and from ordinary standalone file delivery. This source
note does not claim that a candidate has been installed or live acceptance passed.
