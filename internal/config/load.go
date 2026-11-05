package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Foncifelo/sidecar-proxy/internal/logging"
)

// Overrides are command-line values that win over the file. A nil field means
// the flag was not given.
type Overrides struct {
	Mode          *Mode
	Listen        *string
	MetricsListen *string
	InboundToken  *logging.Secret
}

// apply returns a copy of c with the overrides set.
func (o Overrides) apply(c Config) Config {
	out := c
	if o.Mode != nil {
		out.Mode = *o.Mode
	}
	if o.Listen != nil {
		out.Listen = *o.Listen
	}
	if o.MetricsListen != nil {
		out.MetricsListen = *o.MetricsListen
	}
	if o.InboundToken != nil {
		out.InboundToken = *o.InboundToken
	}
	return out
}

// Options tells Load where to read from.
type Options struct {
	// Path to sidecar.yaml.
	Path string
	// PathExplicit is true when the user named the file (--config); a missing
	// file is then always an error.
	PathExplicit bool
	// LookupEnv resolves ${VAR}; nil means os.LookupEnv.
	LookupEnv LookupFunc
	// Overrides from flags.
	Overrides Overrides
}

// ErrNoConfig is returned when the file does not exist and the flags alone are
// not enough to run.
var ErrNoConfig = errors.New("no configuration")

// Load reads the YAML at o.Path, expands ${VAR}, applies flag overrides and
// defaults, and validates. If the file does not exist, was not named
// explicitly, and --mode was given, the config is built from flags and
// defaults only (no upstream).
func Load(o Options) (Config, error) {
	lookup := o.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	env, _ := lookup(EnvVarSidecarEnv)
	allowHTTP := loopbackHTTPAllowed(env)
	data, err := os.ReadFile(o.Path)
	switch {
	case err == nil:
		cfg, err := Parse(data, lookup)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", o.Path, err)
		}
		return finish(o.Overrides.apply(cfg), validateOptions{requireUpstream: true, allowLoopbackHTTP: allowHTTP})
	case !errors.Is(err, fs.ErrNotExist):
		return Config{}, fmt.Errorf("read config: %w", err)
	case o.PathExplicit || o.Overrides.Mode == nil:
		return Config{}, fmt.Errorf("%w: %s does not exist; copy sidecar.example.yaml to %s, or run with --mode (and --listen/--inbound-token as needed)",
			ErrNoConfig, o.Path, o.Path)
	default:
		return finish(o.Overrides.apply(Config{}), validateOptions{requireUpstream: false, allowLoopbackHTTP: allowHTTP})
	}
}

func finish(c Config, o validateOptions) (Config, error) {
	c.LoopbackHTTP = o.allowLoopbackHTTP
	return c.withDefaults().validate(o)
}

// Parse decodes YAML and expands ${VAR} in values. It does not apply defaults
// or validate; Load does. Unknown keys are errors so typos do not pass silently.
func Parse(data []byte, lookup LookupFunc) (Config, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return Config{}, fmt.Errorf("parse yaml: %w", err)
	}
	if root.Kind == 0 {
		return Config{}, nil // empty file
	}
	if err := expandNode(&root, "", lookup); err != nil {
		return Config{}, err
	}
	// Re-encode the expanded tree and decode it strictly; Node.Decode has no
	// KnownFields option.
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	if err := enc.Encode(&root); err != nil {
		return Config{}, fmt.Errorf("re-encode yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return Config{}, fmt.Errorf("re-encode yaml: %w", err)
	}
	dec := yaml.NewDecoder(&buf)
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("decode yaml: %w", redactDecodeError(err))
	}
	return cfg, nil
}

// redactedValue replaces the quoted value in a yaml.v3 type error.
const redactedValue = "`<redacted>`"

// redactDecodeError strips scalar values from yaml.v3 type errors. After
// ${VAR} expansion a value may be a secret, and yaml.v3 quotes up to its first
// seven characters ("cannot unmarshal !!str `sk-supe...` into int"). Each
// entry keeps its line and target type; the span between the first and the
// last backquote becomes <redacted>, so a value with backquotes of its own
// cannot leak either.
func redactDecodeError(err error) error {
	var te *yaml.TypeError
	if !errors.As(err, &te) {
		return err
	}
	entries := make([]string, len(te.Errors))
	for i, e := range te.Errors {
		entries[i] = redactQuoted(e)
	}
	return &yaml.TypeError{Errors: entries}
}

func redactQuoted(s string) string {
	first := strings.IndexByte(s, '`')
	last := strings.LastIndexByte(s, '`')
	if first < 0 || last <= first {
		return s
	}
	return s[:first] + redactedValue + s[last+1:]
}
