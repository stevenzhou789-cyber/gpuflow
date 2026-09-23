# Artifact upload transport

Each uploaded artifact is limited to **1 TiB** (`artifact.MaxSize`). Legacy
multipart requests may add at most **1 MiB** of fields, headers and framing. The
enterprise TLS gateway applies the matching artifact-route limit and disables
request buffering so large model files do not consume gateway temporary disk.
Other API routes keep their existing request limits.

Current Agents stream each file directly to `POST /v1/jobs/{id}/artifacts`:

- `Content-Type: application/octet-stream`
- `Content-Length`: the exact file size, including zero for an empty file
- `Content-Disposition: attachment; filename="artifacts.tar.gz"` (RFC 2231
  filename encoding is supported for Unicode names)
- The existing `node_id` query parameter, Agent session header, attempt token
  header and authorization credential remain required.

Older clients using `multipart/form-data` with one `file` part remain supported.
The API reads multipart parts as streams; it never calls `ParseMultipartForm`
or writes a complete upload to a control-plane temporary file. Additional files,
incomplete framing, short bodies, invalid filenames and oversize files are
rejected before publication.

The S3 adapter uses at most one reusable 128 MiB part buffer for large uploads
(plus a 64 KiB prefix when the legacy client supplies no file size). The 128 MiB
part size keeps a 1 TiB object below S3's 10,000-part limit. Account for concurrent
uploads when sizing control-plane memory. The backing object store must support
multipart upload, multipart copy, abort, read and delete operations in addition
to ordinary PUT/GET; requests above the former 1 GiB limit no longer fail at the
API or bundled enterprise proxy.

Uploads go to a unique hidden version. Publication happens only after the whole
stream and its HTTP framing have been validated, storage promotion completes,
and the current Agent session and execution attempt are revalidated. Files up
to 5 GiB use atomic S3 copy; larger files use server-side multipart copy and
become visible only after completion and durable publication of the job's
artifact reference. Failed multipart copies are aborted and unpublished staging
objects are discarded. A delayed upload from an earlier attempt cannot replace
the current attempt's result.

This transport does not resume at a byte offset. Agent retry reopens its retained
local file and sends the complete upload again. A lost response after publication
can leave an extra immutable version; only the current published reference is
visible, and normal job artifact deletion removes historical versions as well.
Agent retry timeouts still need to cover the chosen file size and network speed.
