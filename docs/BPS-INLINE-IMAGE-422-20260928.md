# Inline user-image 422

The one-shot encrypted production capture `f5e5e543aa43f75eeb651f146f75941c` contained a complete 422 response and 205 input items. The only image was a valid 2048×822 PNG data URI in a user message. Tool calls and results paired correctly; no duplicate IDs or malformed base64 were found. Provider errors still supplied no validation path.

Bounded synthetic controls used a newly generated blue PNG, the same account and model, and no customer history:

| Request | Result |
| --- | --- |
| User inline image, missing `detail` | 422 |
| Same inline image, `detail: auto` | 422 |
| Pure text baseline | 200, completed, OK |
| Valid 512×512 image as user data URI | 422 |
| Upload the same PNG through `/attachments`, then user `input_image.file_id` | 200, completed, OK |

The synthetic uploaded attachment was deleted successfully after that diagnostic. The controls disprove the earlier hypothesis that merely adding `detail` fixes the issue.

The captured official plugin bundle independently explains the difference. User upload/paste paths POST a multipart `file` to `/basispoints/api/attachments`, read the observed `openai_file_id`, and put that ID in user image content. Native screenshot tools may use data URIs in their tool outputs. These are separate protocol locations and must not be conflated.

The implementation uploads inline user/developer/system image content after account admission, skips WebSocket prewarm, and replaces only the outgoing image part with the real `file_id`. It keeps original image bytes/history available for later preparation. It never converts a picture into invented tool output and never discards a picture to force a successful response.

An in-memory one-hour cache stores only file IDs, scoped by API Key, account identity, upstream, proxy fingerprint, image MIME and content hash. Concurrent equal uploads share one flight. Unauthorized/failed/cancelled uploads are not cached or automatically retried. Existing file references and tool-output images retain their original shape. Arbitrary remote image URLs are not fetched by this change. Explicit client-owned `file_id` history cannot migrate to another account.

The code accepts normal message objects with an explicit `type` or its valid omitted form, and handles escaped JSON field names. Multiple images update the prepared request only after all uploads succeed. Original large JSON integers and unrelated context are retained.

Evidence is under `.local/image-detail-probe/`, `.local/capture-release-20260928/capture-items-structure.json`, and `.local/capture-release-20260928/official-attachment-contract.json`. Final live gateway validation additionally checks actual blue-image recognition and that two requests upload only once. Deployment and remaining error counts must be verified separately.
