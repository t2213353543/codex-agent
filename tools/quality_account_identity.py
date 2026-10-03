"""Pure preflight checks for controlled multi-account quality experiments.

The three hashes are deliberately validated independently.  ``identity_sha256``
is the stable account identity supplied by the account source; it is not a
claim about a hardware fingerprint.  ``token_sha256`` identifies the current
credential epoch and must not be folded into the account identity tuple.
``workspace_sha256`` is required metadata, but different users may legitimately
share one workspace and therefore it is not a distinctness key.
"""

from __future__ import annotations

from collections.abc import Iterable, Mapping
import re
from typing import Any


_HASH = re.compile(r"^[0-9a-f]{64}$")
_FIELDS = ("identity_sha256", "token_sha256", "workspace_sha256")


def _rows(epochs: Any) -> list[Mapping[str, Any]] | None:
    """Normalize the supported mapping/list forms without consuming strings."""

    if isinstance(epochs, Mapping):
        values = list(epochs.values())
    elif isinstance(epochs, Iterable) and not isinstance(epochs, (str, bytes, bytearray)):
        values = list(epochs)
    else:
        return None
    if not values or not all(isinstance(value, Mapping) for value in values):
        return None
    return values


def require_distinct_accounts(epochs: Any, allow_single: bool = False) -> bool:
    """Return whether ``epochs`` is safe for a controlled account comparison.

    Rejection is intentionally represented by ``False`` so callers can perform
    this check before writing any credential material or starting a model
    process.  Empty input is never valid.  A single well-formed row is valid
    only for an explicitly marked capture-only operation.
    """

    if not isinstance(allow_single, bool):
        return False
    rows = _rows(epochs)
    if rows is None or (len(rows) < 2 and not allow_single):
        return False

    identities: set[str] = set()
    tokens: set[str] = set()
    for row in rows:
        values = [row.get(field) for field in _FIELDS]
        if any(not isinstance(value, str) or _HASH.fullmatch(value) is None for value in values):
            return False
        identity, token, _workspace = values
        # Token hashes represent credential epochs, not account identity.  They
        # are nevertheless required to be unique so one epoch cannot be used
        # to stand in for two accounts in a supposedly concurrent comparison.
        if identity in identities or token in tokens:
            return False
        identities.add(identity)
        tokens.add(token)
    return True


__all__ = ["require_distinct_accounts"]
