// Command nn-webhooks is the official command-line interface for NimbusNexus Webhooks
// (webhookd). It wraps the Webhooks Go SDK client and speaks the same v1 API.
//
// Configuration (base URL + API key) is resolved in order:
//  1. the global --url / --api-key flags,
//  2. the NN_WEBHOOKS_URL / NN_WEBHOOKS_API_KEY environment variables,
//  3. the config file at $XDG_CONFIG_HOME/nn-webhooks/credentials.json (written by `nn-webhooks configure`).
//
// Successful results are printed as indented JSON to stdout; errors go to stderr and
// the process exits non-zero. An *webhooks.APIError is rendered as "code: message".
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"

	webhooks "github.com/NimbusNexus/webhooks-go"
)

func main() {
	// Global flags precede the subcommand: `nn-webhooks [--url U] [--api-key K] <cmd> ...`.
	// The parser stops at the first non-flag token (the subcommand), so subcommand-local
	// flags (including `endpoints create --url`, which is the endpoint URL) never collide.
	globals := flag.NewFlagSet("nn-webhooks", flag.ContinueOnError)
	globals.Usage = func() { printMainUsage(os.Stderr) }
	urlFlag := globals.String("url", "", "Webhooks base URL (overrides env/config)")
	keyFlag := globals.String("api-key", "", "API key (overrides env/config)")
	profileFlag := globals.String("profile", "", "named credential profile (default \"default\")")
	if err := globals.Parse(os.Args[1:]); err != nil {
		if err == flag.ErrHelp {
			os.Exit(0)
		}
		os.Exit(2)
	}

	args := globals.Args()
	if len(args) == 0 {
		printMainUsage(os.Stderr)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "help", "-h", "--help":
		printMainUsage(os.Stdout)
		return
	case "version":
		err = cmdVersion()
	case "verify":
		cmdVerify(rest) // handles its own exit codes (0 ok / 1 failed / 2 usage)
		return
	case "configure":
		err = cmdConfigure(rest, *urlFlag, *keyFlag, *profileFlag)
	case "whoami":
		err = cmdWhoami(*urlFlag, *keyFlag, *profileFlag)
	case "publish":
		err = cmdPublish(ctx, rest, *urlFlag, *keyFlag, *profileFlag)
	case "endpoints":
		err = cmdEndpoints(ctx, rest, *urlFlag, *keyFlag, *profileFlag)
	case "keys":
		err = cmdKeys(ctx, rest, *urlFlag, *keyFlag, *profileFlag)
	case "deliveries":
		err = cmdDeliveries(ctx, rest, *urlFlag, *keyFlag, *profileFlag)
	default:
		fmt.Fprintf(os.Stderr, "nn-webhooks: unknown command %q\n\n", cmd)
		printMainUsage(os.Stderr)
		os.Exit(2)
	}

	if err != nil {
		fail(err)
	}
}

// fail prints err to stderr and exits non-zero. An *APIError renders as "code: message".
func fail(err error) {
	var apiErr *webhooks.APIError
	if errors.As(err, &apiErr) {
		fmt.Fprintf(os.Stderr, "%s: %s\n", apiErr.Code, apiErr.Message)
	} else {
		fmt.Fprintf(os.Stderr, "nn-webhooks: %s\n", err.Error())
	}
	os.Exit(1)
}

// ---------------------------------------------------------------------------
// version
// ---------------------------------------------------------------------------

// Set by the release build via -ldflags; empty for a plain `go build` or `go install`.
//
// Both are reported, because they answer different questions and can legitimately differ: `version`
// is the RELEASE this binary came from, `sdk` is the client library compiled into it. A binary
// built from an untagged commit has no release to name, so it falls back to the SDK constant
// rather than printing "dev" and losing the only real information available.
var (
	version string
	commit  string
)

func cmdVersion() error {
	v := version
	if v == "" {
		v = webhooks.Version + "+source"
	}
	out := map[string]string{"version": v, "sdk": webhooks.Version}
	if commit != "" {
		out["commit"] = commit
	}
	return printJSON(out)
}

// ---------------------------------------------------------------------------
// verify
// ---------------------------------------------------------------------------

func cmdVerify(args []string) {
	fs := newFlagSet("nn-webhooks verify", "Usage: nn-webhooks verify --secret S --signature SIG [--timestamp TS]\n\nReads the raw webhook body from stdin, prints \"ok\"/\"failed\", and exits 0/1.")
	secret := fs.String("secret", "", "signing secret (required)")
	signature := fs.String("signature", "", "X-Webhook-Signature header value (required)")
	timestamp := fs.String("timestamp", "", "X-Webhook-Timestamp header value (unix seconds)")
	mustParse(fs, args)

	if *secret == "" || *signature == "" {
		fmt.Fprintln(os.Stderr, "nn-webhooks verify: --secret and --signature are required")
		fs.Usage()
		os.Exit(2)
	}

	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nn-webhooks verify: failed to read body from stdin: %v\n", err)
		os.Exit(2)
	}

	var opts *webhooks.VerifyOptions
	if *timestamp != "" {
		ts, perr := strconv.ParseInt(*timestamp, 10, 64)
		if perr != nil {
			// A malformed timestamp verifies as false, never a panic.
			fmt.Println("failed")
			os.Exit(1)
		}
		opts = &webhooks.VerifyOptions{Timestamp: &ts}
	}

	if webhooks.Verify(*secret, body, *signature, opts) {
		fmt.Println("ok")
		os.Exit(0)
	}
	fmt.Println("failed")
	os.Exit(1)
}

// ---------------------------------------------------------------------------
// configure
// ---------------------------------------------------------------------------

func cmdConfigure(args []string, gURL, gKey, gProfile string) error {
	fs := newFlagSet("nn-webhooks configure", "Usage: nn-webhooks configure [--url URL] [--api-key KEY]\n\nWrites the base URL + API key to $XDG_CONFIG_HOME/nn-webhooks/credentials.json (mode 0600).\nMissing values are prompted for interactively.")
	urlFlag := fs.String("url", "", "Webhooks base URL")
	keyFlag := fs.String("api-key", "", "API key")
	mustParse(fs, args)

	baseURL := firstNonEmpty(*urlFlag, gURL)
	apiKey := firstNonEmpty(*keyFlag, gKey)

	reader := bufio.NewReader(os.Stdin)
	if baseURL == "" {
		v, err := promptLine(reader, "Webhooks base URL: ")
		if err != nil {
			return err
		}
		baseURL = v
	}
	if apiKey == "" {
		v, err := promptLine(reader, "API key: ")
		if err != nil {
			return err
		}
		apiKey = v
	}
	if baseURL == "" || apiKey == "" {
		return errors.New("both base URL and API key are required")
	}

	// Writes one PROFILE rather than a flat file, so a second deployment is `--profile staging`
	// instead of overwriting the first — which is how a production key ends up aimed at staging.
	name := profileName(gProfile)
	st, err := loadStore()
	if err != nil {
		return err
	}
	st.Profiles[name] = profile{URL: baseURL, Kind: "api_key", APIKey: apiKey}
	path, err := saveStore(st)
	if err != nil {
		return err
	}
	return printJSON(map[string]string{"url": baseURL, "profile": name, "config_path": path})
}

// cmdWhoami reports which credential would be used, and WHERE it came from.
//
// The SOURCE is the point, not the fact. A stale $NN_WEBHOOKS_API_KEY silently shadowing the
// profile someone just wrote is the common confusion, and "configured" on its own cannot explain
// it — the precedence that makes CI work with no config file is the same precedence that makes
// this happen.
//
// The key itself is never printed, only its last four characters: the output of a diagnostic
// command is exactly what gets pasted into an issue or a chat log.
func cmdWhoami(gURL, gKey, gProfile string) error {
	cred, err := resolveCredentials(gURL, gKey, gProfile)
	if err != nil {
		return err
	}
	suffix := cred.APIKey
	if len(suffix) > 4 {
		suffix = suffix[len(suffix)-4:]
	}
	return printJSON(map[string]string{
		"profile":    cred.Profile,
		"url":        cred.URL,
		"kind":       cred.Kind,
		"source":     cred.Source,
		"key_suffix": suffix,
	})
}

// ---------------------------------------------------------------------------
// publish
// ---------------------------------------------------------------------------

func cmdPublish(ctx context.Context, args []string, gURL, gKey, gProfile string) error {
	usage := "Usage: nn-webhooks publish <event_type> [--data JSON] [--idempotency-key K] [--project-id ID] [--source S]"
	fs := newFlagSet("nn-webhooks publish", usage)
	data := fs.String("data", "", "event payload as a JSON object")
	idempotency := fs.String("idempotency-key", "", "Idempotency-Key header")
	projectID := fs.String("project-id", "", "project id, e.g. prj_3f9a… (default: the workspace's default project)")
	source := fs.String("source", "", "event source")

	// event_type is the leading positional; a flag-first invocation still routes --help
	// through the FlagSet (showing every flag) before failing on the missing argument.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		mustParse(fs, args)
		fmt.Fprintln(os.Stderr, "nn-webhooks publish: missing <event_type> argument")
		fs.Usage()
		os.Exit(2)
	}
	eventType := args[0]
	mustParse(fs, args[1:])

	payload := map[string]any{}
	if *data != "" {
		if err := json.Unmarshal([]byte(*data), &payload); err != nil {
			return fmt.Errorf("publish: --data must be a JSON object: %w", err)
		}
	}

	opts := &webhooks.PublishOptions{}
	if *projectID != "" {
		opts.ProjectID = *projectID
	}
	if flagProvided(fs, "source") {
		s := *source
		opts.Source = &s
	}
	if *idempotency != "" {
		opts.IdempotencyKey = *idempotency
	}

	client, err := newClient(gURL, gKey, gProfile)
	if err != nil {
		return err
	}
	ev, err := client.Publish(ctx, eventType, payload, opts)
	if err != nil {
		return err
	}
	return printJSON(ev)
}

// ---------------------------------------------------------------------------
// endpoints
// ---------------------------------------------------------------------------

func cmdEndpoints(ctx context.Context, args []string, gURL, gKey, gProfile string) error {
	if len(args) == 0 {
		printEndpointsUsage(os.Stderr)
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "-h", "--help", "help":
		printEndpointsUsage(os.Stdout)
		return nil
	case "list":
		return endpointsList(ctx, rest, gURL, gKey, gProfile)
	case "get":
		return endpointsGet(ctx, rest, gURL, gKey, gProfile)
	case "create":
		return endpointsCreate(ctx, rest, gURL, gKey, gProfile)
	case "update":
		return endpointsUpdate(ctx, rest, gURL, gKey, gProfile)
	case "delete":
		return endpointsDelete(ctx, rest, gURL, gKey, gProfile)
	case "rotate-secret":
		return endpointsRotateSecret(ctx, rest, gURL, gKey, gProfile)
	case "enable":
		return endpointsEnable(ctx, rest, gURL, gKey, gProfile)
	default:
		fmt.Fprintf(os.Stderr, "nn-webhooks endpoints: unknown subcommand %q\n\n", sub)
		printEndpointsUsage(os.Stderr)
		os.Exit(2)
	}
	return nil
}

func endpointsList(ctx context.Context, args []string, gURL, gKey, gProfile string) error {
	fs := newFlagSet("nn-webhooks endpoints list", "Usage: nn-webhooks endpoints list [--project-id ID] [--limit N] [--offset N]")
	projectID := fs.String("project-id", "", "project id, e.g. prj_3f9a… (default: the workspace's default project)")
	limit := fs.Int("limit", 0, "maximum results")
	offset := fs.Int("offset", 0, "pagination offset")
	mustParse(fs, args)

	opts := &webhooks.ListEndpointsOptions{Offset: *offset}
	if *projectID != "" {
		opts.ProjectID = *projectID
	}
	if *limit > 0 {
		l := *limit
		opts.Limit = &l
	}

	client, err := newClient(gURL, gKey, gProfile)
	if err != nil {
		return err
	}
	page, err := client.ListEndpoints(ctx, opts)
	if err != nil {
		return err
	}
	return printJSON(page)
}

func endpointsGet(ctx context.Context, args []string, gURL, gKey, gProfile string) error {
	fs := newFlagSet("nn-webhooks endpoints get", "Usage: nn-webhooks endpoints get <id>")
	id, client, err := requireIDAndClient(fs, args, gURL, gKey, gProfile)
	if err != nil {
		return err
	}
	ep, err := client.GetEndpoint(ctx, id)
	if err != nil {
		return err
	}
	return printJSON(ep)
}

func endpointsCreate(ctx context.Context, args []string, gURL, gKey, gProfile string) error {
	fs := newFlagSet("nn-webhooks endpoints create", "Usage: nn-webhooks endpoints create --url URL [--project-id ID] [--subscribe kind:pattern]... [--max-attempts N] [--description D]")
	epURL := fs.String("url", "", "endpoint target URL (required)")
	projectID := fs.String("project-id", "", "project id, e.g. prj_3f9a… (default: the workspace's default project)")
	var subs stringList
	fs.Var(&subs, "subscribe", "subscription as kind:pattern (repeatable)")
	maxAttempts := fs.Int("max-attempts", 0, "maximum delivery attempts")
	description := fs.String("description", "", "endpoint description")
	mustParse(fs, args)

	if *epURL == "" {
		fmt.Fprintln(os.Stderr, "nn-webhooks endpoints create: --url is required")
		fs.Usage()
		os.Exit(2)
	}

	opts := &webhooks.CreateEndpointOptions{}
	if *projectID != "" {
		opts.ProjectID = *projectID
	}
	for _, s := range subs {
		opts.Subscriptions = append(opts.Subscriptions, parseSubscription(s))
	}
	if flagProvided(fs, "max-attempts") {
		m := *maxAttempts
		opts.MaxAttempts = &m
	}
	if flagProvided(fs, "description") {
		d := *description
		opts.Description = &d
	}

	client, err := newClient(gURL, gKey, gProfile)
	if err != nil {
		return err
	}
	ep, err := client.CreateEndpoint(ctx, *epURL, opts)
	if err != nil {
		return err
	}
	return printJSON(ep)
}

func endpointsUpdate(ctx context.Context, args []string, gURL, gKey, gProfile string) error {
	fs := newFlagSet("nn-webhooks endpoints update", "Usage: nn-webhooks endpoints update <id> --set key=value [--set key=value]...\n\nEach value is JSON-coerced (falling back to a string); use null to clear a field.")
	var sets stringList
	fs.Var(&sets, "set", "field update as key=value (repeatable, JSON-coerced)")
	id, client, err := requireIDAndClient(fs, args, gURL, gKey, gProfile)
	if err != nil {
		return err
	}
	if len(sets) == 0 {
		return errors.New("endpoints update: at least one --set key=value is required")
	}

	patch := webhooks.Patch{}
	for _, kv := range sets {
		eq := strings.Index(kv, "=")
		if eq < 0 {
			return fmt.Errorf("endpoints update: invalid --set %q (want key=value)", kv)
		}
		key := kv[:eq]
		raw := kv[eq+1:]
		if key == "" {
			return fmt.Errorf("endpoints update: invalid --set %q (empty key)", kv)
		}
		patch[key] = coerceJSON(raw)
	}

	ep, err := client.UpdateEndpoint(ctx, id, patch)
	if err != nil {
		return err
	}
	return printJSON(ep)
}

func endpointsDelete(ctx context.Context, args []string, gURL, gKey, gProfile string) error {
	fs := newFlagSet("nn-webhooks endpoints delete", "Usage: nn-webhooks endpoints delete <id>")
	id, client, err := requireIDAndClient(fs, args, gURL, gKey, gProfile)
	if err != nil {
		return err
	}
	if err := client.DeleteEndpoint(ctx, id); err != nil {
		return err
	}
	return printJSON(map[string]string{"id": id, "status": "deleted"})
}

func endpointsRotateSecret(ctx context.Context, args []string, gURL, gKey, gProfile string) error {
	fs := newFlagSet("nn-webhooks endpoints rotate-secret", "Usage: nn-webhooks endpoints rotate-secret <id>")
	id, client, err := requireIDAndClient(fs, args, gURL, gKey, gProfile)
	if err != nil {
		return err
	}
	ep, err := client.RotateEndpointSecret(ctx, id)
	if err != nil {
		return err
	}
	return printJSON(ep)
}

func endpointsEnable(ctx context.Context, args []string, gURL, gKey, gProfile string) error {
	fs := newFlagSet("nn-webhooks endpoints enable", "Usage: nn-webhooks endpoints enable <id>")
	id, client, err := requireIDAndClient(fs, args, gURL, gKey, gProfile)
	if err != nil {
		return err
	}
	ep, err := client.EnableEndpoint(ctx, id)
	if err != nil {
		return err
	}
	return printJSON(ep)
}

// ---------------------------------------------------------------------------
// keys
// ---------------------------------------------------------------------------

func cmdKeys(ctx context.Context, args []string, gURL, gKey, gProfile string) error {
	if len(args) == 0 {
		printKeysUsage(os.Stderr)
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "-h", "--help", "help":
		printKeysUsage(os.Stdout)
		return nil
	case "create":
		return keysCreate(ctx, rest, gURL, gKey, gProfile)
	case "revoke":
		return keysRevoke(ctx, rest, gURL, gKey, gProfile)
	default:
		fmt.Fprintf(os.Stderr, "nn-webhooks keys: unknown subcommand %q\n\n", sub)
		printKeysUsage(os.Stderr)
		os.Exit(2)
	}
	return nil
}

func keysCreate(ctx context.Context, args []string, gURL, gKey, gProfile string) error {
	fs := newFlagSet("nn-webhooks keys create", "Usage: nn-webhooks keys create [--name N] [--scope admin|publish|read] [--expires-in-days N]\n\nCalls POST /v1/api-keys, which webhookd has deleted: this fails with a 404. Mint keys in your\nNimbusNexus account console under \"API keys\", choosing Webhooks as the product.")
	name := fs.String("name", "", "key name")
	scope := fs.String("scope", "", "scope: admin|publish (default admin)")
	expires := fs.Int("expires-in-days", 0, "expiry in days")
	mustParse(fs, args)

	opts := &webhooks.CreateAPIKeyOptions{}
	if *name != "" {
		opts.Name = *name
	}
	if *scope != "" {
		opts.Scope = *scope
	}
	if flagProvided(fs, "expires-in-days") {
		e := *expires
		opts.ExpiresInDays = &e
	}

	client, err := newClient(gURL, gKey, gProfile)
	if err != nil {
		return err
	}
	key, err := client.CreateAPIKey(ctx, opts)
	if err != nil {
		return err
	}
	return printJSON(key)
}

func keysRevoke(ctx context.Context, args []string, gURL, gKey, gProfile string) error {
	fs := newFlagSet("nn-webhooks keys revoke", "Usage: nn-webhooks keys revoke <id>\n\nCalls DELETE /v1/api-keys/{id}, which webhookd has deleted: this fails with a 404. Revoke keys in\nyour NimbusNexus account console.")
	id, client, err := requireIDAndClient(fs, args, gURL, gKey, gProfile)
	if err != nil {
		return err
	}
	if err := client.RevokeAPIKey(ctx, id); err != nil {
		return err
	}
	return printJSON(map[string]string{"id": id, "status": "revoked"})
}

// ---------------------------------------------------------------------------
// deliveries
// ---------------------------------------------------------------------------

func cmdDeliveries(ctx context.Context, args []string, gURL, gKey, gProfile string) error {
	if len(args) == 0 {
		printDeliveriesUsage(os.Stderr)
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "-h", "--help", "help":
		printDeliveriesUsage(os.Stdout)
		return nil
	case "list":
		return deliveriesList(ctx, rest, gURL, gKey, gProfile)
	case "redeliver":
		return deliveriesRedeliver(ctx, rest, gURL, gKey, gProfile)
	default:
		fmt.Fprintf(os.Stderr, "nn-webhooks deliveries: unknown subcommand %q\n\n", sub)
		printDeliveriesUsage(os.Stderr)
		os.Exit(2)
	}
	return nil
}

func deliveriesList(ctx context.Context, args []string, gURL, gKey, gProfile string) error {
	fs := newFlagSet("nn-webhooks deliveries list", "Usage: nn-webhooks deliveries list [--status S] [--endpoint ID] [--event-type T] [--since TS] [--until TS] [--q Q] [--limit N] [--offset N]")
	status := fs.String("status", "", "delivery status filter")
	endpoint := fs.String("endpoint", "", "endpoint id filter")
	eventType := fs.String("event-type", "", "event type filter")
	since := fs.String("since", "", "created-at lower bound")
	until := fs.String("until", "", "created-at upper bound")
	q := fs.String("q", "", "free-text search")
	limit := fs.Int("limit", 0, "maximum results")
	offset := fs.Int("offset", 0, "pagination offset")
	mustParse(fs, args)

	opts := &webhooks.ListDeliveriesOptions{
		Status:     *status,
		EndpointID: *endpoint,
		EventType:  *eventType,
		Since:      *since,
		Until:      *until,
		Q:          *q,
	}
	if *limit > 0 {
		l := *limit
		opts.Limit = &l
	}
	if flagProvided(fs, "offset") {
		o := *offset
		opts.Offset = &o
	}

	client, err := newClient(gURL, gKey, gProfile)
	if err != nil {
		return err
	}
	page, err := client.ListDeliveries(ctx, opts)
	if err != nil {
		return err
	}
	return printJSON(page)
}

func deliveriesRedeliver(ctx context.Context, args []string, gURL, gKey, gProfile string) error {
	fs := newFlagSet("nn-webhooks deliveries redeliver", "Usage: nn-webhooks deliveries redeliver <id>")
	id, client, err := requireIDAndClient(fs, args, gURL, gKey, gProfile)
	if err != nil {
		return err
	}
	d, err := client.Redeliver(ctx, id)
	if err != nil {
		return err
	}
	return printJSON(d)
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// stringList is a flag.Value that accumulates repeated flag values (e.g. --subscribe, --set).
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// newFlagSet builds a ContinueOnError FlagSet whose Usage prints the given line plus flag defaults.
func newFlagSet(name, usage string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), usage)
		fs.PrintDefaults()
	}
	return fs
}

// mustParse parses args, exiting 0 on -h/--help and 2 on any other parse error (the FlagSet has
// already printed the message and usage in both cases).
func mustParse(fs *flag.FlagSet, args []string) {
	err := fs.Parse(args)
	if err == nil {
		return
	}
	if err == flag.ErrHelp {
		os.Exit(0)
	}
	os.Exit(2)
}

// requireIDAndClient extracts a leading positional <id>, parses any remaining flags (honouring
// --help), and builds a configured client. Used by every "<verb> <id>" subcommand.
func requireIDAndClient(fs *flag.FlagSet, args []string, gURL, gKey, gProfile string) (string, *webhooks.Client, error) {
	var id string
	flagArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id = args[0]
		flagArgs = args[1:]
	}
	mustParse(fs, flagArgs)
	if id == "" {
		fmt.Fprintf(fs.Output(), "%s: missing <id> argument\n", fs.Name())
		fs.Usage()
		os.Exit(2)
	}
	client, err := newClient(gURL, gKey, gProfile)
	if err != nil {
		return "", nil, err
	}
	return id, client, nil
}

// flagProvided reports whether the named flag was explicitly set on the command line. It lets us
// distinguish "not given" from "given the zero value" for optional pointer fields.
func flagProvided(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// parseSubscription turns a "kind:pattern" argument into a Subscription. A value with no colon is
// treated as a bare match kind (e.g. "all") with an empty pattern.
func parseSubscription(s string) webhooks.Subscription {
	if idx := strings.Index(s, ":"); idx >= 0 {
		return webhooks.Subscription{MatchKind: s[:idx], Pattern: s[idx+1:]}
	}
	return webhooks.Subscription{MatchKind: s}
}

// coerceJSON parses raw as a JSON value, falling back to the raw string when it is not valid JSON.
// This lets `--set max_attempts=5` send a number, `--set description=null` clear the field, and
// `--set description=hello` send a plain string.
func coerceJSON(raw string) any {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err == nil {
		return v
	}
	return raw
}

// newClient resolves the base URL + API key and returns a configured SDK client.
func newClient(gURL, gKey, gProfile string) (*webhooks.Client, error) {
	cred, err := resolveCredentials(gURL, gKey, gProfile)
	if err != nil {
		return nil, err
	}
	return webhooks.New(cred.URL, cred.APIKey), nil
}

// resolveConfig applies the flag -> env -> config-file precedence for the base URL and API key.
// promptLine writes label to stderr and reads one trimmed line from r.
func promptLine(r *bufio.Reader, label string) (string, error) {
	fmt.Fprint(os.Stderr, label)
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// printJSON writes v to stdout as indented JSON followed by a newline.
func printJSON(v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if _, err := os.Stdout.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// usage text
// ---------------------------------------------------------------------------

func printMainUsage(w io.Writer) {
	fmt.Fprint(w, `nn-webhooks - CLI for NimbusNexus Webhooks (webhookd)

Usage:
  nn-webhooks [--url URL] [--api-key KEY] <command> [arguments]

Global flags:
  --url URL         Webhooks base URL (env NN_WEBHOOKS_URL, or config file)
  --api-key KEY     API key           (env NN_WEBHOOKS_API_KEY, or config file)
  --profile NAME    Named credential profile (env NN_WEBHOOKS_PROFILE, default "default")

Commands:
  configure     Save base URL + API key to the credentials file (see below)
  whoami        Show the resolved URL + key, and which source supplied each
  publish       Publish an event
  endpoints     Manage endpoints (list, get, create, update, delete, rotate-secret, enable)
  keys          API-key commands — webhookd deleted these routes; they now fail (404)
  deliveries    Inspect deliveries (list, redeliver)
  verify        Verify a webhook signature (body read from stdin)
  version       Print the SDK/CLI version

Run "nn-webhooks <command> --help" for command-specific help.

Configuration is resolved in order: flags, then the NN_WEBHOOKS_URL / NN_WEBHOOKS_API_KEY
environment variables, then $XDG_CONFIG_HOME/nn-webhooks/credentials.json — falling back to
~/.config/nn-webhooks/credentials.json when XDG_CONFIG_HOME is unset. Run whoami to see which
source won.
`)
}

func printEndpointsUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: nn-webhooks endpoints <subcommand> [arguments]

Subcommands:
  list                                 List endpoints
  get <id>                             Fetch an endpoint
  create --url URL [flags]             Create an endpoint
  update <id> --set key=value ...      Update an endpoint (JSON-coerced values)
  delete <id>                          Delete an endpoint
  rotate-secret <id>                   Rotate the signing secret
  enable <id>                          Re-enable a disabled endpoint

Run "nn-webhooks endpoints <subcommand> --help" for details.
`)
}

func printKeysUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: nn-webhooks keys <subcommand> [arguments]

webhookd has deleted the /v1/api-keys routes, so both subcommands below fail with a 404. Mint and
revoke keys in your NimbusNexus account console under "API keys", choosing Webhooks as the product.

Subcommands:
  create [--name N] [--scope admin|publish|read] [--expires-in-days N]   Calls the deleted create route
  revoke <id>                                                       Calls the deleted revoke route

Run "nn-webhooks keys <subcommand> --help" for details.
`)
}

func printDeliveriesUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: nn-webhooks deliveries <subcommand> [arguments]

Subcommands:
  list [filters]      List deliveries (--status, --endpoint, --event-type, --since, --until, --q, --limit)
  redeliver <id>      Re-enqueue a delivery for another attempt

Run "nn-webhooks deliveries <subcommand> --help" for details.
`)
}
