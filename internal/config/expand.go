package config

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// LookupFunc resolves an environment variable; os.LookupEnv satisfies it.
type LookupFunc func(name string) (string, bool)

// varRef matches ${NAME}. Anything else (including a bare $NAME) stays literal.
var varRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// MissingVarError reports a ${VAR} reference whose variable is not set. It names
// the YAML key and the variable, never a value.
type MissingVarError struct {
	Key string // dotted path, e.g. "upstream.api_key"
	Var string
}

func (e *MissingVarError) Error() string {
	return fmt.Sprintf("%s: environment variable %s is not set", e.Key, e.Var)
}

// expandNode replaces ${VAR} in every scalar value of the tree rooted at n.
// Mapping keys and comments are never expanded. All missing variables are
// reported together.
func expandNode(n *yaml.Node, path string, lookup LookupFunc) error {
	switch n.Kind {
	case yaml.DocumentNode:
		errs := make([]error, 0, len(n.Content))
		for _, c := range n.Content {
			errs = append(errs, expandNode(c, path, lookup))
		}
		return errors.Join(errs...)
	case yaml.MappingNode:
		errs := make([]error, 0, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			errs = append(errs, expandNode(n.Content[i+1], joinPath(path, n.Content[i].Value), lookup))
		}
		return errors.Join(errs...)
	case yaml.SequenceNode:
		errs := make([]error, 0, len(n.Content))
		for i, c := range n.Content {
			errs = append(errs, expandNode(c, path+"["+strconv.Itoa(i)+"]", lookup))
		}
		return errors.Join(errs...)
	case yaml.ScalarNode:
		return expandScalar(n, path, lookup)
	case yaml.AliasNode:
		return nil // the anchored node is expanded where it is defined
	default:
		return nil
	}
}

func expandScalar(n *yaml.Node, path string, lookup LookupFunc) error {
	if !strings.Contains(n.Value, "${") {
		return nil
	}
	var errs []error
	n.Value = varRef.ReplaceAllStringFunc(n.Value, func(ref string) string {
		name := ref[2 : len(ref)-1]
		v, ok := lookup(name)
		if !ok {
			errs = append(errs, &MissingVarError{Key: path, Var: name})
			return ""
		}
		return v
	})
	// A plain scalar is re-resolved after expansion so that numbers such as
	// max_inflight: ${MAX} decode into ints. Quoted scalars stay strings.
	if n.Style == 0 {
		n.Tag = ""
	}
	return errors.Join(errs...)
}

func joinPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}
