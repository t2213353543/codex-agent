package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type managedIngressQuery struct {
	From        int64  `json:"from"`
	To          int64  `json:"to"`
	Status      int    `json:"status"`
	Reason      string `json:"reason"`
	Correlation string `json:"client_request_id"`
	Limit       int    `json:"limit"`
	Cursor      string `json:"-"`
}
type managedIngressCursor struct {
	Epoch   string `json:"epoch"`
	Ceiling int64  `json:"ceiling"`
	Before  int64  `json:"before"`
	Floor   int64  `json:"floor"`
	Filter  string `json:"filter"`
}

func parseManagedIngressQuery(values url.Values) (managedIngressQuery, error) {
	q := managedIngressQuery{Limit: 50}
	for key, items := range values {
		if len(items) != 1 || !stringIn(key, "from", "to", "status", "reason", "client_request_id", "limit", "cursor") {
			return q, errInvalid
		}
	}
	for key, target := range map[string]*int64{"from": &q.From, "to": &q.To} {
		if value := values.Get(key); value != "" {
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n <= 0 || n > 253402300799 {
				return q, errInvalid
			}
			*target = n
		}
	}
	if q.To != 0 && q.To <= q.From {
		return q, errInvalid
	}
	for key, target := range map[string]*int{"status": &q.Status, "limit": &q.Limit} {
		if value := values.Get(key); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil {
				return q, errInvalid
			}
			*target = n
		}
	}
	if q.Limit < 1 || q.Limit > 200 || values.Get("status") != "" && (q.Status < 400 || q.Status > 599) {
		return q, errInvalid
	}
	q.Reason, q.Correlation, q.Cursor = values.Get("reason"), values.Get("client_request_id"), values.Get("cursor")
	if q.Reason != "" && gatewayIngressReasonStatus(q.Reason) == 0 || q.Correlation != "" && !validGatewayIngressCorrelation(q.Correlation) || len(q.Cursor) > 2048 {
		return q, errInvalid
	}
	return q, nil
}
func (q managedIngressQuery) fingerprint() string {
	q.Cursor = ""
	raw, _ := json.Marshal(q)
	return adminKeyHash(string(raw))
}
func (q managedIngressQuery) matches(event gatewayIngressEvent) bool {
	return (q.From == 0 || event.TimeMs >= q.From*1000) && (q.To == 0 || event.TimeMs < q.To*1000) && (q.Status == 0 || event.HTTPCode == q.Status) && (q.Reason == "" || event.Reason == q.Reason) && (q.Correlation == "" || event.ClientRequestID == q.Correlation)
}
func (p *Proxy) handleManagedIngressEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if p.Store.DB == nil && p.Store.Path == "" {
		adminAPIError(w, 503, "ingress_storage_unavailable", "ingress observation storage is not configured")
		return
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		adminError(w, errInvalid)
		return
	}
	q, err := parseManagedIngressQuery(values)
	if err != nil {
		adminError(w, err)
		return
	}
	var cursor managedIngressCursor
	if q.Cursor != "" {
		raw, e := base64.RawURLEncoding.DecodeString(q.Cursor)
		if e != nil {
			adminError(w, errInvalid)
			return
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&cursor) != nil || decoder.Decode(new(any)) != io.EOF || cursor.Epoch == "" || cursor.Ceiling < 1 || cursor.Before < 1 || cursor.Before > cursor.Ceiling || cursor.Floor < 1 || cursor.Floor > cursor.Before || cursor.Filter != q.fingerprint() {
			adminError(w, errInvalid)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	entry, err := p.managementState().Get(ctx, "ingress-events", "recent")
	if err != nil && !errors.Is(err, errNotFound) {
		p.storeError(w, err)
		return
	}
	doc, err := decodeGatewayIngressDocument(entry)
	if err != nil {
		p.storeError(w, err)
		return
	}
	floor := doc.NextSequence + 1
	if len(doc.Events) > 0 {
		floor = doc.Events[0].Sequence
	}
	if q.Cursor != "" && (doc.Epoch != cursor.Epoch || doc.NextSequence < cursor.Ceiling || floor > cursor.Floor) {
		adminAPIError(w, 409, "ingress_retention_changed", "retained ingress events changed; restart pagination")
		return
	}
	if q.Cursor == "" {
		cursor = managedIngressCursor{Epoch: doc.Epoch, Ceiling: doc.NextSequence, Before: doc.NextSequence + 1, Floor: floor, Filter: q.fingerprint()}
	}
	items := []gatewayIngressEvent{}
	total := 0
	for i := len(doc.Events) - 1; i >= 0; i-- {
		event := doc.Events[i]
		if event.Sequence > cursor.Ceiling || !q.matches(event) {
			continue
		}
		total++
		if event.Sequence < cursor.Before && len(items) < q.Limit+1 {
			items = append(items, event)
		}
	}
	hasMore := len(items) > q.Limit
	next := ""
	if hasMore {
		items = items[:q.Limit]
		cursor.Before = items[len(items)-1].Sequence
		raw, _ := json.Marshal(cursor)
		next = base64.RawURLEncoding.EncodeToString(raw)
	}
	writeJSON(w, map[string]any{"items": items, "scope": "retained_ingress_events", "source": gatewayIngressSource, "total": total, "total_kind": "exact", "has_more": hasMore, "next_cursor": next, "checked_at": time.Now().Unix(), "retention_limit": gatewayIngressRetentionLimit, "evicted_total": doc.EvictedTotal, "collector": p.IngressEvents.status()})
}
