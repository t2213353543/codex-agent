package server

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type legacyImportMarker struct {
	Manifest  string `json:"manifest"`
	Completed bool   `json:"completed"`
}

type legacyBundle struct {
	Accounts accountState
	Groups   groupState
	Proxies  proxyState
	Keys     keyState
	Settings settingsState
	Stats    statsState
	AdminKey string
	LogsPath string
	Manifest string
	Summary  migrationSummary
}
type migrationSummary struct {
	DryRun         bool   `json:"dry_run"`
	Manifest       string `json:"manifest"`
	Accounts       int    `json:"accounts"`
	Groups         int    `json:"groups"`
	Proxies        int    `json:"proxies"`
	Keys           int    `json:"keys"`
	AccountGroups  int    `json:"account_groups"`
	AccountProxies int    `json:"account_proxies"`
	Logs           int    `json:"logs"`
	LogRequests    int64  `json:"log_requests"`
	LogTokens      int64  `json:"log_tokens"`
	StatsRequests  int64  `json:"stats_requests"`
	StatsTokens    int64  `json:"stats_tokens"`
	MetadataHash   string `json:"metadata_hash"`
	Validation     string `json:"validation"`
}

func runStorageCommand(args []string) error {
	command := args[0]
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	source := fs.String("source", "data", "legacy directory or JSON/JSONL credential file")
	target := fs.String("target", "", "new directory for current legacy export")
	dryRun := fs.Bool("dry-run", false, "validate without any database writes")
	before := fs.String("before", "", "retention cutoff RFC3339; defaults to 92 days ago")
	analyticsBatchSize := fs.Int("batch-size", 1000, "analytics maintenance rows per transaction (1-1000)")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("unexpected command arguments")
	}
	switch command {
	case "migrate", "import-legacy", "export-legacy", "rebuild-usage", "prune-logs", "runtime-status", "repair-official-usage", "backfill-log-analytics":
	default:
		return errors.New("unknown command; use migrate, import-legacy, export-legacy, rebuild-usage or prune-logs")
	}
	var bundle legacyBundle
	var err error
	if command == "import-legacy" {
		bundle, err = readLegacyBundle(*source)
		if err != nil {
			return err
		}
		bundle.Summary.DryRun = *dryRun
		if *dryRun {
			return json.NewEncoder(os.Stdout).Encode(bundle.Summary)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	db, err := databaseFromEnvironment(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	if command == "migrate" {
		if err = db.Migrate(ctx); err != nil {
			return err
		}
		fmt.Println(`{"migrated":true}`)
		return nil
	}
	if err = db.requireSchema(ctx); err != nil {
		return err
	}
	switch command {
	case "backfill-log-analytics":
		if *dryRun {
			return errors.New("backfill-log-analytics requires explicit execution; dry-run is unsupported")
		}
		result, err := db.BackfillLogAnalytics(ctx, *analyticsBatchSize)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	case "repair-official-usage":
		return repairOfficialUsage(ctx, db, *source, *dryRun)
	case "runtime-status":
		// Release health needs the current database key after operator rotation;
		// never export the key to a shell, log or workstation.
		p := &Proxy{Store: Store{DB: db}}
		key, err := p.currentAdminKey(ctx)
		if err != nil || key == "" {
			return errors.New("current management credential unavailable")
		}
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:18879/api/settings", nil)
		request.Header.Set("Authorization", "Bearer "+key)
		client := newHTTPClient()
		client.Timeout = 10 * time.Second
		response, err := client.Do(request)
		if err != nil {
			return errors.New("runtime settings check failed")
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return errors.New("runtime settings authentication failed")
		}
		var data map[string]json.RawMessage
		if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&data) != nil {
			return errors.New("invalid runtime settings response")
		}
		out := map[string]json.RawMessage{}
		for _, field := range []string{"version", "settings_version", "inference_mode", "inference", "websocket_enabled"} {
			if v, ok := data[field]; ok {
				out[field] = v
			}
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	case "import-legacy":
		if err = importLegacyBundle(ctx, db, bundle); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(bundle.Summary)
	case "export-legacy":
		if *target == "" {
			return errors.New("--target is required and must not exist")
		}
		return exportLegacyBundle(ctx, db, *target)
	case "rebuild-usage":
		m, err := NewPGMetrics(db)
		if err != nil {
			return err
		}
		if err = m.RebuildAggregates(ctx); err != nil {
			return err
		}
		fmt.Println(`{"rebuilt":true}`)
	case "prune-logs":
		cutoff := time.Now().AddDate(0, 0, -metricsRetentionDays)
		if *before != "" {
			cutoff, err = time.Parse(time.RFC3339, *before)
			if err != nil {
				return errors.New("invalid --before RFC3339")
			}
		}
		m, err := NewPGMetrics(db)
		if err != nil {
			return err
		}
		var total int64
		for {
			n, err := m.Prune(ctx, cutoff, 5000)
			if err != nil {
				return err
			}
			total += n
			if n < 5000 {
				break
			}
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"deleted_logs": total, "before": cutoff.Format(time.RFC3339), "durable_events_retained": true})
	}
	return nil
}

func readLegacyBundle(source string) (legacyBundle, error) {
	b := legacyBundle{Settings: settingsState{AdminAPI: true}}
	info, err := os.Stat(source)
	if err != nil {
		return b, errors.New("legacy source unavailable")
	}
	hash := sha256.New()
	if !info.IsDir() {
		raw, err := os.ReadFile(source)
		if err != nil {
			return b, err
		}
		if len(raw) > adminBodyLimit {
			return b, errors.New("credential input exceeds 32 MiB")
		}
		hash.Write(raw)
		items, err := ParseImportInput(raw)
		if err != nil {
			return b, err
		}
		for _, item := range items {
			a, err := parseAccount(item.Name, item.Credentials)
			if err != nil {
				return b, err
			}
			a = applyAccountDefaults(a, AccountDefaults{})
			a.ID = legacyAccountID(a.Name)
			b.Accounts.Accounts = append(b.Accounts.Accounts, a)
		}
	} else {
		docs := []struct {
			name   string
			target any
		}{{"accounts.json", &b.Accounts}, {"groups.json", &b.Groups}, {"proxies.json", &b.Proxies}, {"keys.json", &b.Keys}, {"settings.json", &b.Settings}, {"stats.json", &b.Stats}}
		for _, doc := range docs {
			raw, err := os.ReadFile(filepath.Join(source, doc.name))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return b, fmt.Errorf("cannot read %s", doc.name)
			}
			if len(raw) > adminBodyLimit {
				return b, fmt.Errorf("%s exceeds 32 MiB", doc.name)
			}
			hash.Write([]byte(doc.name))
			hash.Write(raw)
			trimmed := strings.TrimSpace(string(raw))
			if len(trimmed) == 0 || trimmed[0] != '{' || json.Unmarshal(raw, doc.target) != nil {
				return b, fmt.Errorf("invalid JSON in %s", doc.name)
			}
		}
		raw, err := os.ReadFile(filepath.Join(source, "client-key"))
		if err == nil {
			b.AdminKey = strings.TrimSpace(string(raw))
			if len(b.AdminKey) < 32 {
				return b, errors.New("legacy client-key is invalid")
			}
			hash.Write([]byte("client-key"))
			hash.Write(raw)
		} else if !errors.Is(err, os.ErrNotExist) {
			return b, errors.New("cannot read legacy client-key")
		}
		b.LogsPath = filepath.Join(source, "logs.jsonl")
		if err = scanLegacyLogs(b.LogsPath, func(line int, raw []byte, rec RequestRecord) error {
			hash.Write(raw)
			hash.Write([]byte{'\n'})
			b.Summary.Logs++
			b.Summary.LogRequests++
			b.Summary.LogTokens += rec.tokens()
			return nil
		}); err != nil {
			return b, err
		}
	}
	groupNames := map[string]bool{}
	groups := map[string]bool{}
	proxies := map[string]bool{}
	names := map[string]bool{}
	ids := map[string]bool{}
	fingerprints := map[string]bool{}
	for _, g := range b.Groups.Groups {
		if g.ID == "" || groups[g.ID] || groupNames[g.Name] {
			return b, errors.New("duplicate or empty legacy group ID")
		}
		if err = validGroupName(g.Name); err != nil {
			return b, err
		}
		groups[g.ID] = true
		groupNames[g.Name] = true
	}
	for _, p := range b.Proxies.Proxies {
		if p.ID == "" || proxies[p.ID] {
			return b, errors.New("duplicate or empty legacy proxy ID")
		}
		if err = validProxy(p); err != nil {
			return b, err
		}
		proxies[p.ID] = true
	}
	metadata := []any{}
	for i := range b.Accounts.Accounts {
		a := &b.Accounts.Accounts[i]
		if err = validAccountName(a.Name); err != nil {
			return b, err
		}
		if names[a.Name] {
			return b, errors.New("duplicate legacy account name")
		}
		names[a.Name] = true
		// Reuse the same strict token validation as the management importer.
		raw, _ := json.Marshal(a)
		if _, err = parseAccount(a.Name, raw); err != nil {
			return b, fmt.Errorf("invalid credentials for legacy account index %d", i)
		}
		fp := hex.EncodeToString(tokenFingerprint(a.AccessToken))
		if fingerprints[fp] {
			return b, errors.New("duplicate legacy credential; resolve before migration")
		}
		fingerprints[fp] = true
		normalizeAccount(a)
		if a.ID == "" {
			a.ID = legacyAccountID(a.Name)
		}
		if ids[a.ID] {
			return b, errors.New("duplicate legacy account ID")
		}
		ids[a.ID] = true
		for _, id := range a.Groups {
			if !groups[id] {
				return b, errors.New("legacy account refers to missing group")
			}
			b.Summary.AccountGroups++
		}
		for _, id := range a.Proxies {
			if !proxies[id] {
				return b, errors.New("legacy account refers to missing proxy")
			}
			b.Summary.AccountProxies++
		}
		metadata = append(metadata, struct {
			ID, Name        string
			Expiry          int64
			Groups, Proxies []string
		}{a.ID, a.Name, a.ExpiresAt, a.Groups, a.Proxies})
	}
	if b.Accounts.Active != "" && !names[b.Accounts.Active] {
		return b, errors.New("legacy active account is missing")
	}
	keyIDs := map[string]bool{}
	keyValues := map[string]bool{}
	for _, k := range b.Keys.Keys {
		if k.ID == "" || k.Key == "" || keyIDs[k.ID] || keyValues[k.Key] {
			return b, errors.New("invalid or duplicate legacy API key")
		}
		keyIDs[k.ID] = true
		keyValues[k.Key] = true
		for _, id := range k.Groups {
			if !groups[id] {
				return b, errors.New("legacy key refers to missing group")
			}
		}
	}
	for _, counts := range b.Stats.Minutes {
		if counts == nil {
			continue
		}
		b.Summary.StatsRequests += counts.Requests
		b.Summary.StatsTokens += counts.Tokens
	}
	b.Manifest = hex.EncodeToString(hash.Sum(nil))
	b.Summary.Manifest = b.Manifest
	b.Summary.Accounts = len(b.Accounts.Accounts)
	b.Summary.Groups = len(groups)
	b.Summary.Proxies = len(proxies)
	b.Summary.Keys = len(b.Keys.Keys)
	raw, _ := json.Marshal(metadata)
	sum := sha256.Sum256(raw)
	b.Summary.MetadataHash = hex.EncodeToString(sum[:])
	b.Summary.Validation = "counts, identities, references and expiries validated"
	return b, nil
}
func scanLegacyLogs(path string, visit func(int, []byte, RequestRecord) error) error {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("cannot open legacy logs")
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	line := 0
	for scanner.Scan() {
		line++
		raw := scanner.Bytes()
		if len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}
		var rec RequestRecord
		if json.Unmarshal(raw, &rec) != nil || rec.Time <= 0 || rec.Status == "" || rec.InputTokens < 0 || rec.OutputTokens < 0 || rec.CachedTokens < 0 {
			return fmt.Errorf("invalid logs.jsonl at line %d", line)
		}
		if err = visit(line, raw, rec); err != nil {
			return err
		}
	}
	if scanner.Err() != nil {
		return fmt.Errorf("cannot read logs.jsonl at line %d", line+1)
	}
	return nil
}

func importLegacyBundle(ctx context.Context, db *Database, b legacyBundle) error {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return safeDBError(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || ':legacy_import',712498302))"); err != nil {
		return safeDBError(err)
	}
	var prior legacyImportMarker
	if err = db.readDocument(ctx, tx, "legacy_import", &prior); err != nil {
		return err
	}
	if prior.Manifest != "" && prior.Manifest != b.Manifest {
		return errors.New("different legacy source already imported; use a fresh database")
	}
	if prior.Manifest == "" {
		var populated bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM accounts UNION ALL SELECT 1 FROM groups UNION ALL SELECT 1 FROM proxies UNION ALL SELECT 1 FROM usage_events)`).Scan(&populated); err != nil {
			return safeDBError(err)
		}
		if populated {
			return errors.New("legacy import requires an empty database")
		}
		for _, g := range b.Groups.Groups {
			_, err = tx.Exec(ctx, `INSERT INTO groups(id,name,description,created_at) VALUES($1,$2,$3,to_timestamp($4))`, g.ID, g.Name, g.Description, g.CreatedAt)
			if err != nil {
				return safeDBError(err)
			}
		}
		for _, p := range b.Proxies.Proxies {
			encrypted := []byte{}
			if p.Password != "" {
				encrypted, err = db.Encrypt([]byte(p.Password))
				if err != nil {
					return err
				}
			}
			_, err = tx.Exec(ctx, `INSERT INTO proxies(id,name,type,host,port,username,password_cipher,enabled,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,to_timestamp($9))`, p.ID, p.Name, p.Type, p.Host, p.Port, p.Username, encrypted, p.Enabled, p.CreatedAt)
			if err != nil {
				return safeDBError(err)
			}
		}
		if err = (Store{DB: db}).ImportLegacyAccounts(ctx, tx, b.Accounts); err != nil {
			return err
		}
		for _, doc := range []struct {
			key   string
			value any
		}{{"gateway_keys", b.Keys}, {"admin_settings", b.Settings}, {"legacy_import", struct {
			Manifest string `json:"manifest"`
		}{b.Manifest}}} {
			if err = db.writeDocument(ctx, tx, doc.key, doc.value); err != nil {
				return err
			}
		}
		if b.AdminKey != "" {
			if err = db.writeDocument(ctx, tx, "admin_key", b.AdminKey); err != nil {
				return err
			}
		}
		if err = db.audit(ctx, tx, "legacy.import", b.Manifest); err != nil {
			return safeDBError(err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return safeDBError(err)
	}
	m, err := NewPGMetrics(db)
	if err != nil {
		return err
	}
	batch := make([]RequestRecord, 0, 250)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		_, _, err := m.ImportRecords(ctx, batch)
		batch = batch[:0]
		return err
	}
	if err = scanLegacyLogs(b.LogsPath, func(line int, raw []byte, rec RequestRecord) error {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", b.Manifest, line)))
		rec.EventID = "legacy_" + hex.EncodeToString(sum[:])
		batch = append(batch, rec)
		if len(batch) >= 250 {
			return flush()
		}
		return nil
	}); err != nil {
		return err
	}
	if err = flush(); err != nil {
		return err
	}
	// Legacy snapshots may contain usage whose detailed log was already removed.
	if err = importLegacyStatsChecked(ctx, m, b.Stats, b.Manifest); err != nil {
		return err
	}
	var accounts, relations, proxyRelations int
	if err = db.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM accounts),(SELECT count(*) FROM account_groups),(SELECT count(*) FROM account_proxies)`).Scan(&accounts, &relations, &proxyRelations); err != nil {
		return safeDBError(err)
	}
	if accounts != b.Summary.Accounts || relations != b.Summary.AccountGroups || proxyRelations != b.Summary.AccountProxies {
		return errors.New("post-import association/count validation failed")
	}
	done, err := db.Pool.Begin(ctx)
	if err != nil {
		return safeDBError(err)
	}
	defer done.Rollback(ctx)
	if err = db.writeDocument(ctx, done, "legacy_import", legacyImportMarker{Manifest: b.Manifest, Completed: true}); err != nil {
		return err
	}
	return safeDBError(done.Commit(ctx))
}

func exportLegacyBundle(ctx context.Context, db *Database, target string) error {
	absolute, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	if _, err = os.Stat(absolute); !errors.Is(err, os.ErrNotExist) {
		return errors.New("export target must not exist")
	}
	active, accounts, err := (Store{DB: db}).List()
	if err != nil {
		return err
	}
	names := map[string]bool{}
	for _, a := range accounts {
		if names[a.Name] {
			return errors.New("duplicate display names prevent legacy export")
		}
		names[a.Name] = true
	}
	groups, err := NewPGGroupStore(db).List()
	if err != nil {
		return err
	}
	proxies, err := NewPGProxyStore(db)
	if err != nil {
		return err
	}
	proxyEntries, err := proxies.List()
	if err != nil {
		return err
	}
	keys, err := NewPGKeyStore(db)
	if err != nil {
		return err
	}
	keyEntries, err := keys.List()
	if err != nil {
		return err
	}
	settings, err := NewPGSettingsStore(db).Get()
	if err != nil {
		return err
	}
	var key string
	if err = db.readDocument(ctx, db.Pool, "admin_key", &key); err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		return err
	}
	temp, err := os.MkdirTemp(filepath.Dir(absolute), ".legacy-export-")
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			os.RemoveAll(temp)
		}
	}()
	documents := map[string]any{"accounts.json": accountState{Active: active, Accounts: accounts}, "groups.json": groupState{Groups: groups}, "proxies.json": proxyState{Proxies: proxyEntries}, "keys.json": keyState{Keys: keyEntries}, "settings.json": settings.state()}
	fileNames := make([]string, 0, len(documents))
	for name := range documents {
		fileNames = append(fileNames, name)
	}
	sort.Strings(fileNames)
	for _, name := range fileNames {
		raw, err := json.MarshalIndent(documents[name], "", "  ")
		if err != nil {
			return err
		}
		if err = writePrivateFile(filepath.Join(temp, name), append(raw, '\n')); err != nil {
			return err
		}
	}
	if key != "" {
		if err = writePrivateFile(filepath.Join(temp, "client-key"), []byte(key)); err != nil {
			return err
		}
	}
	m, err := NewPGMetrics(db)
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(temp, "logs.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = m.ExportRecords(ctx, logFile)
	syncErr := logFile.Sync()
	closeErr := logFile.Close()
	if err = errors.Join(err, syncErr, closeErr); err != nil {
		return err
	}
	statsFile, err := os.OpenFile(filepath.Join(temp, "stats.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = m.ExportLegacyStats(ctx, statsFile)
	syncErr = statsFile.Sync()
	closeErr = statsFile.Close()
	if err = errors.Join(err, syncErr, closeErr); err != nil {
		return err
	}

	if err = os.Rename(temp, absolute); err != nil {
		return err
	}
	complete = true
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"exported": true, "accounts": len(accounts), "groups": len(groups), "proxies": len(proxyEntries), "keys": len(keyEntries)})
}
func writePrivateFile(path string, raw []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, e1 := f.Write(raw)
	e2 := f.Sync()
	e3 := f.Close()
	return errors.Join(e1, e2, e3)
}
