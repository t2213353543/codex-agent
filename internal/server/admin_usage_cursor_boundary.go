package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Ordinary v1 cursors remain self-contained. Only huge model boundaries use
// a compact v2 record reference; if cleanup or a repair changes that boundary,
// reject the cursor explicitly rather than silently skipping different rows.
func (m *Metrics) resolveManagedModelBoundary(ctx context.Context, c *managedLogCursor) error {
	if c.Version != 2 {
		return nil
	}
	bad := fmt.Errorf("%w: model cursor boundary changed or was removed; restart pagination", errInvalid)
	var model string
	if m.db != nil {
		if c.Storage != "postgres" {
			return errInvalid
		}
		var stamp time.Time
		err := m.db.Pool.QueryRow(ctx, "SELECT model,created_at FROM request_logs WHERE id=$1", c.ID).Scan(&model, &stamp)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return bad
			}
			return err
		}
		// Imported records may preserve a fractional stored timestamp while
		// the public record has integer seconds. Seek using the actual indexed
		// timestamp, and reject a boundary moved to a different whole second.
		if stamp.Unix() != c.Time {
			return bad
		}
		c.ExactTime = &stamp
	} else {
		if c.Storage != "file" {
			return errInvalid
		}
		found := false
		_, err := m.walkManagedFile(ctx, c.Ceiling, func(r RequestRecord) error {
			if r.ID == c.ID && r.Time == c.Time {
				model = r.Model
				found = true
			}
			return nil
		})
		if err != nil {
			return err
		}
		if !found {
			return bad
		}
	}
	digest := sha256.Sum256([]byte(model))
	if hex.EncodeToString(digest[:]) != c.ModelHash {
		return bad
	}
	c.Text = model
	return nil
}
