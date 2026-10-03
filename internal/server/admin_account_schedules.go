package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
)

type managedCron struct {
	fields           [5]map[int]bool
	domStar, dowStar bool
	location         *time.Location
}

func parseManagedCron(expression, timezone string) (managedCron, error) {
	var cron managedCron
	parts := strings.Fields(expression)
	if len(parts) != 5 || len(expression) > 256 {
		return cron, errors.New("cron requires five minute/hour/day/month/weekday fields")
	}
	if timezone == "" {
		timezone = "UTC"
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return cron, errors.New("unknown IANA timezone")
	}
	cron.location = location
	mins := []int{0, 0, 1, 1, 0}
	maxs := []int{59, 23, 31, 12, 7}
	names := []map[string]int{nil, nil, nil, {"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6, "JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12}, {"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6}}
	for i, field := range parts {
		cron.fields[i] = map[int]bool{}
		parse := func(s string) (int, error) {
			if n, ok := names[i][strings.ToUpper(s)]; ok {
				return n, nil
			}
			n, err := strconv.Atoi(s)
			if err != nil || n < mins[i] || n > maxs[i] {
				return 0, errInvalid
			}
			return n, nil
		}
		for _, piece := range strings.Split(field, ",") {
			segments := strings.Split(piece, "/")
			if len(segments) > 2 || segments[0] == "" {
				return cron, errInvalid
			}
			step := 1
			if len(segments) == 2 {
				step, err = strconv.Atoi(segments[1])
				if err != nil || step < 1 || step > maxs[i]-mins[i]+1 {
					return cron, errInvalid
				}
			}
			base := segments[0]
			start, end := mins[i], maxs[i]
			if base != "*" {
				interval := strings.Split(base, "-")
				if len(interval) > 2 {
					return cron, errInvalid
				}
				start, err = parse(interval[0])
				if err != nil {
					return cron, errInvalid
				}
				end = start
				if len(interval) == 2 {
					end, err = parse(interval[1])
					if err != nil || end < start {
						return cron, errInvalid
					}
				} else if len(segments) == 2 {
					end = maxs[i]
				}
			}
			if i == 2 && base == "*" {
				cron.domStar = true
			}
			if i == 4 && base == "*" {
				cron.dowStar = true
			}
			for value := start; value <= end; value += step {
				v := value
				if i == 4 && v == 7 {
					v = 0
				}
				cron.fields[i][v] = true
			}
		}
	}
	return cron, nil
}
func (cron managedCron) matches(at time.Time) bool {
	local := at.In(cron.location)
	if !cron.fields[0][local.Minute()] || !cron.fields[1][local.Hour()] || !cron.fields[3][int(local.Month())] {
		return false
	}
	dom, dow := cron.fields[2][local.Day()], cron.fields[4][int(local.Weekday())]
	if !cron.domStar && !cron.dowStar {
		return dom || dow
	}
	return dom && dow
}
func (cron managedCron) next(after time.Time) (int64, error) {
	when := after.Truncate(time.Minute).Add(time.Minute)
	deadline := when.AddDate(5, 0, 0)
	for when.Before(deadline) {
		if cron.matches(when) {
			return when.Unix(), nil
		}
		when = when.Add(time.Minute)
	}
	return 0, errors.New("cron has no occurrence in the next five years")
}

type managedAccountSchedule struct {
	ID         string `json:"id"`
	AccountID  string `json:"account_id"`
	Version    int64  `json:"version"`
	Kind       string `json:"kind"`
	Model      string `json:"model,omitempty"`
	Cron       string `json:"cron"`
	Timezone   string `json:"timezone"`
	Enabled    bool   `json:"enabled"`
	MaxResults int    `json:"max_results"`
	NextRunAt  int64  `json:"next_run_at"`
	LastRunAt  int64  `json:"last_run_at"`
	Running    bool   `json:"running"`
}

// errManagedScheduleNoChange aborts a schedule document update whose merged
// plan equals the stored plan: no document write, no audit, no version bump.
var errManagedScheduleNoChange = errors.New("managed schedule unchanged")

// unchangedFrom compares every plan field a PUT can set, except Version.
// LastRunAt/Running belong to the scheduler, not the update handler.
func (s managedAccountSchedule) unchangedFrom(other managedAccountSchedule) bool {
	return s.Kind == other.Kind && s.Model == other.Model && s.Cron == other.Cron &&
		s.Timezone == other.Timezone && s.Enabled == other.Enabled &&
		s.MaxResults == other.MaxResults && s.NextRunAt == other.NextRunAt
}
type managedScheduleRun struct {
	ID          string                   `json:"id"`
	ScheduledAt int64                    `json:"scheduled_at"`
	StartedAt   int64                    `json:"started_at"`
	FinishedAt  int64                    `json:"finished_at"`
	Status      string                   `json:"status"`
	Snapshot    *managedUpstreamSnapshot `json:"snapshot,omitempty"`
}
type managedScheduleActive struct {
	Run        managedScheduleRun `json:"run"`
	LeaseUntil int64              `json:"lease_until"`
}
type managedScheduleState struct {
	Plan    managedAccountSchedule `json:"plan"`
	Active  *managedScheduleActive `json:"active,omitempty"`
	Results []managedScheduleRun   `json:"results"`
}
type managedScheduleDocument struct {
	Plans    map[string]*managedScheduleState  `json:"plans"`
	Requests map[string]managedScheduleRequest `json:"requests,omitempty"`
}

type managedScheduleRequest struct {
	Hash   string `json:"hash"`
	PlanID string `json:"plan_id"`
}

var managedScheduleRequestID = regexp.MustCompile(`^[A-Za-z0-9_-]{10,100}$`)

type managedScheduleInput struct {
	RequestID        string `json:"request_id,omitempty"`
	Version          int64  `json:"version"`
	Kind             string `json:"kind"`
	Model            string `json:"model"`
	Cron             string `json:"cron"`
	Timezone         string `json:"timezone"`
	Enabled          bool   `json:"enabled"`
	MaxResults       int    `json:"max_results"`
	ConfirmInference bool   `json:"confirm_inference"`
}

func validateManagedSchedule(input *managedScheduleInput) (int64, error) {
	if !accountHas([]string{"stream", "compact", "usage", "models"}, input.Kind) {
		return 0, errors.New("unsupported scheduled check kind")
	}
	if input.Kind == "stream" || input.Kind == "compact" {
		if !validManagedModel(input.Model) || input.Enabled && !input.ConfirmInference {
			return 0, errors.New("scheduled inference requires an exact model and confirm_inference=true when enabled")
		}
	} else {
		input.Model = ""
	}
	if input.MaxResults == 0 {
		input.MaxResults = 20
	}
	if input.MaxResults < 1 || input.MaxResults > 100 {
		return 0, errors.New("max_results must be 1 to 100")
	}
	if input.Timezone == "" {
		input.Timezone = "UTC"
	}
	cron, err := parseManagedCron(input.Cron, input.Timezone)
	if err != nil {
		return 0, err
	}
	return cron.next(time.Now())
}
func decodeManagedSchedules(entry *managementStateEntry) (managedScheduleDocument, error) {
	doc := managedScheduleDocument{Plans: map[string]*managedScheduleState{}}
	if entry.Version > 0 {
		if err := json.Unmarshal(entry.Payload, &doc); err != nil {
			return doc, err
		}
		if doc.Plans == nil {
			doc.Plans = map[string]*managedScheduleState{}
		}
	}
	return doc, nil
}
func managedScheduleAccountID(planID string) (string, error) {
	id, _, found := strings.Cut(planID, "_s_")
	if !found || id == "" || len(planID) > 128 {
		return "", errInvalid
	}
	return id, nil
}
func (p *Proxy) handleManagedScheduleList(w http.ResponseWriter, r *http.Request) {
	a, err := p.Store.GetMetadata(r.PathValue("name"))
	if err != nil {
		p.storeError(w, err)
		return
	}
	if a.ID != r.PathValue("name") {
		p.storeError(w, errNotFound)
		return
	}
	entry, err := p.managementState().Get(r.Context(), "account-schedules", a.ID)
	if err != nil && !errors.Is(err, errNotFound) {
		p.storeError(w, err)
		return
	}
	doc, err := decodeManagedSchedules(&entry)
	if err != nil {
		p.storeError(w, err)
		return
	}
	plans := []managedAccountSchedule{}
	for _, row := range doc.Plans {
		plans = append(plans, row.Plan)
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].ID < plans[j].ID })
	writeJSON(w, map[string]any{"plans": plans})
}
func (p *Proxy) handleManagedScheduleCreate(w http.ResponseWriter, r *http.Request) {
	var input managedScheduleInput
	if err := readJSONLimit(w, r, &input, 16<<10); err != nil {
		adminError(w, err)
		return
	}
	next, err := validateManagedSchedule(&input)
	if err != nil {
		adminError(w, err)
		return
	}
	if input.Version != 0 {
		adminError(w, errors.New("new schedules must not specify version"))
		return
	}
	if input.RequestID != "" && !managedScheduleRequestID.MatchString(input.RequestID) {
		adminError(w, errInvalid)
		return
	}
	normalized := input
	normalized.RequestID = ""
	raw, _ := json.Marshal(normalized)
	requestHash := managedCRSHash(string(raw))
	a, err := p.Store.GetMetadata(r.PathValue("name"))
	if err != nil {
		p.storeError(w, err)
		return
	}
	if a.ID != r.PathValue("name") {
		p.storeError(w, errNotFound)
		return
	}
	plan := managedAccountSchedule{ID: a.ID + "_s_" + managedOAuthRandom()[:16], AccountID: a.ID, Version: 1, Kind: input.Kind, Model: input.Model, Cron: input.Cron, Timezone: input.Timezone, Enabled: input.Enabled, MaxResults: input.MaxResults, NextRunAt: next}
	err = p.managementState().UpdateAudited(r.Context(), "account-schedules", a.ID, "account.schedule.create", func(entry *managementStateEntry) error {
		current, err := p.Store.GetMetadata(a.ID)
		if err != nil {
			return err
		}
		if current.ID != a.ID {
			return errNotFound
		}
		doc, err := decodeManagedSchedules(entry)
		if err != nil {
			return err
		}
		if input.RequestID != "" {
			if previous, exists := doc.Requests[input.RequestID]; exists {
				if previous.Hash != requestHash || doc.Plans[previous.PlanID] == nil {
					return errConflict
				}
				plan = doc.Plans[previous.PlanID].Plan
				return errManagedScheduleReplay
			}
			if len(doc.Requests) >= 1000 {
				return errors.Join(errInvalid, errors.New("schedule request history limit reached"))
			}
			if doc.Requests == nil {
				doc.Requests = map[string]managedScheduleRequest{}
			}
			doc.Requests[input.RequestID] = managedScheduleRequest{Hash: requestHash, PlanID: plan.ID}
		}
		if len(doc.Plans) >= 20 {
			return fmtManagedScheduleLimit()
		}
		doc.Plans[plan.ID] = &managedScheduleState{Plan: plan, Results: []managedScheduleRun{}}
		entry.Payload, err = json.Marshal(doc)
		return err
	})
	if err != nil && !errors.Is(err, errManagedScheduleReplay) {
		p.storeError(w, err)
		return
	}
	writeJSON(w, plan)
}

var errManagedScheduleReplay = errors.New("scheduled creation request already committed")

func (p *Proxy) managedScheduleAccountExists(ctx context.Context, id string) error {
	a, err := p.Store.GetMetadata(id)
	if err == nil && a.ID == id {
		return nil
	}
	if err == nil {
		err = errNotFound
	}
	return err
}

func (p *Proxy) cleanupManagedDeletedAccountSchedules(ctx context.Context, id string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if errors.Is(p.managedScheduleAccountExists(cleanupCtx, id), errNotFound) {
		_ = p.managementState().Delete(cleanupCtx, "account-schedules", id, 0)
	}
}
func fmtManagedScheduleLimit() error {
	return errors.Join(errInvalid, errors.New("at most 20 scheduled plans per account"))
}
func (p *Proxy) handleManagedScheduleUpdate(w http.ResponseWriter, r *http.Request) {
	var input managedScheduleInput
	if err := readJSONLimit(w, r, &input, 16<<10); err != nil {
		adminError(w, err)
		return
	}
	if input.Version < 1 {
		adminError(w, errInvalid)
		return
	}
	next, err := validateManagedSchedule(&input)
	if err != nil {
		adminError(w, err)
		return
	}
	id := r.PathValue("id")
	accountID, err := managedScheduleAccountID(id)
	if err != nil {
		adminError(w, err)
		return
	}
	if err := p.managedScheduleAccountExists(r.Context(), accountID); err != nil {
		p.storeError(w, err)
		return
	}
	var result managedAccountSchedule
	err = p.managementState().UpdateAudited(r.Context(), "account-schedules", accountID, "account.schedule.update", func(entry *managementStateEntry) error {
		doc, err := decodeManagedSchedules(entry)
		if err != nil {
			return err
		}
		row := doc.Plans[id]
		if row == nil {
			return errNotFound
		}
		if row.Plan.Version != input.Version {
			return errVersionConflict
		}
		before, resultsBefore := row.Plan, len(row.Results)
		row.Plan.Version++
		row.Plan.Kind = input.Kind
		row.Plan.Model = input.Model
		row.Plan.Cron = input.Cron
		row.Plan.Timezone = input.Timezone
		row.Plan.Enabled = input.Enabled
		row.Plan.MaxResults = input.MaxResults
		row.Plan.NextRunAt = next
		if len(row.Results) > input.MaxResults {
			row.Results = row.Results[:input.MaxResults]
		}
		// A PUT that reproduces the stored plan (same fields, same recomputed
		// next run, no result truncation) rewrites nothing.
		if len(row.Results) == resultsBefore && row.Plan.unchangedFrom(before) {
			result = before
			return errManagedScheduleNoChange
		}
		result = row.Plan
		entry.Payload, err = json.Marshal(doc)
		return err
	})
	if err != nil && !errors.Is(err, errManagedScheduleNoChange) {
		p.storeError(w, err)
		return
	}
	writeJSON(w, result)
}
func (p *Proxy) handleManagedScheduleDelete(w http.ResponseWriter, r *http.Request) {
	version, err := parseVersionQuery(r)
	if err != nil || version < 1 {
		adminError(w, errInvalid)
		return
	}
	id := r.PathValue("id")
	accountID, err := managedScheduleAccountID(id)
	if err != nil {
		adminError(w, err)
		return
	}
	if err := p.managedScheduleAccountExists(r.Context(), accountID); err != nil {
		p.storeError(w, err)
		return
	}
	err = p.managementState().UpdateAudited(r.Context(), "account-schedules", accountID, "account.schedule.delete", func(entry *managementStateEntry) error {
		doc, err := decodeManagedSchedules(entry)
		if err != nil {
			return err
		}
		row := doc.Plans[id]
		if row == nil {
			return errNotFound
		}
		if row.Plan.Version != version {
			return errVersionConflict
		}
		delete(doc.Plans, id)
		entry.Payload, err = json.Marshal(doc)
		return err
	})
	if err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
func (p *Proxy) handleManagedScheduleResults(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if value := r.URL.Query().Get("limit"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 100 {
			adminError(w, errInvalid)
			return
		}
		limit = n
	}
	id := r.PathValue("id")
	accountID, err := managedScheduleAccountID(id)
	if err != nil {
		adminError(w, err)
		return
	}
	if err := p.managedScheduleAccountExists(r.Context(), accountID); err != nil {
		p.storeError(w, err)
		return
	}
	entry, err := p.managementState().Get(r.Context(), "account-schedules", accountID)
	if err != nil {
		p.storeError(w, err)
		return
	}
	doc, err := decodeManagedSchedules(&entry)
	if err != nil {
		p.storeError(w, err)
		return
	}
	row := doc.Plans[id]
	if row == nil {
		p.storeError(w, errNotFound)
		return
	}
	results := row.Results
	if len(results) > limit {
		results = results[:limit]
	}
	if results == nil {
		results = []managedScheduleRun{}
	}
	writeJSON(w, map[string]any{"results": results, "running": row.Active})
}

func appendManagedScheduleResult(row *managedScheduleState, run managedScheduleRun) {
	row.Results = append([]managedScheduleRun{run}, row.Results...)
	if len(row.Results) > row.Plan.MaxResults {
		row.Results = row.Results[:row.Plan.MaxResults]
	}
}
func (p *Proxy) processManagedAccountSchedules(ctx context.Context) {
	p.processManagedAccountLeases(ctx, time.Now())
	p.processManagedAccountSchedulesAt(ctx, time.Now())
}
func (p *Proxy) processManagedAccountSchedulesAt(ctx context.Context, now time.Time) {
	entries, err := p.managementState().List(ctx, "account-schedules")
	if err != nil {
		return
	}
	type candidate struct {
		accountID, planID string
		next              int64
	}
	candidates := []candidate{}
	for accountID, entry := range entries {
		if err := p.managedScheduleAccountExists(ctx, accountID); err != nil {
			if errors.Is(err, errNotFound) {
				_ = p.managementState().Delete(ctx, "account-schedules", accountID, entry.Version)
			}
			continue
		}
		doc, err := decodeManagedSchedules(&entry)
		if err != nil {
			continue
		}
		for id, row := range doc.Plans {
			if row.Active != nil && row.Active.LeaseUntil <= now.Unix() || row.Plan.Enabled && row.Plan.NextRunAt <= now.Unix() {
				candidates = append(candidates, candidate{accountID, id, row.Plan.NextRunAt})
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].next == candidates[j].next {
			return candidates[i].planID < candidates[j].planID
		}
		return candidates[i].next < candidates[j].next
	})
	if len(candidates) > 4 {
		candidates = candidates[:4]
	}
	var wg sync.WaitGroup
	for _, task := range candidates {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(c candidate) { defer wg.Done(); p.runManagedSchedule(ctx, c.accountID, c.planID, now) }(task)
	}
	wg.Wait()
}

func (p *Proxy) runManagedSchedule(ctx context.Context, accountID, id string, now time.Time) {
	var plan managedAccountSchedule
	var run managedScheduleRun
	claimed := false
	err := p.managementState().Update(ctx, "account-schedules", accountID, func(entry *managementStateEntry) error {
		doc, err := decodeManagedSchedules(entry)
		if err != nil {
			return err
		}
		row := doc.Plans[id]
		if row == nil {
			return errNotFound
		}
		if row.Active != nil {
			if row.Active.LeaseUntil > now.Unix() {
				return errConflict
			}
			previous := row.Active.Run
			previous.Status = "unknown"
			previous.FinishedAt = now.Unix()
			appendManagedScheduleResult(row, previous)
			row.Active = nil
			row.Plan.Running = false
		}
		if row.Plan.Enabled && row.Plan.NextRunAt <= now.Unix() {
			cron, err := parseManagedCron(row.Plan.Cron, row.Plan.Timezone)
			if err != nil {
				return err
			}
			next, err := cron.next(now)
			if err != nil {
				return err
			}
			plan = row.Plan
			run = managedScheduleRun{ID: managedOAuthRandom(), ScheduledAt: plan.NextRunAt, StartedAt: now.Unix(), Status: "running"}
			row.Active = &managedScheduleActive{Run: run, LeaseUntil: now.Add(90 * time.Second).Unix()}
			row.Plan.NextRunAt = next
			row.Plan.LastRunAt = now.Unix()
			row.Plan.Running = true
			claimed = true
		}
		entry.Payload, err = json.Marshal(doc)
		return err
	})
	if err != nil || !claimed {
		return
	}
	// A second instance can only see the persisted lease and future NextRunAt.
	// An expired lease is finalized as unknown, never replayed.
	a, err := p.Store.Get(accountID)
	if err != nil || !a.Enabled {
		run.Status = "skipped"
	} else {
		checkCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		snapshot := p.checkManagedUpstream(checkCtx, a, plan.Kind, plan.Model)
		cancel()
		run.Snapshot = &snapshot
		run.Status = "failed"
		if snapshot.OK {
			run.Status = "ok"
		}
		saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		_ = p.cacheManagedUpstream(saveCtx, snapshot)
		saveCancel()
	}
	run.FinishedAt = time.Now().Unix()
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = p.managementState().Update(finishCtx, "account-schedules", accountID, func(entry *managementStateEntry) error {
		doc, err := decodeManagedSchedules(entry)
		if err != nil {
			return err
		}
		row := doc.Plans[id]
		if row == nil || row.Active == nil || row.Active.Run.ID != run.ID {
			return errConflict
		}
		row.Active = nil
		row.Plan.Running = false
		appendManagedScheduleResult(row, run)
		entry.Payload, err = json.Marshal(doc)
		return err
	})
}
