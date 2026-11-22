package main

import (
	"fmt"
	"strings"
)

// Decision is the outcome of routing one request.
type Decision struct {
	Backend     string
	ServedModel string
	Rewritten   bool
}

// Resolve applies SPEC.md section 6: role, then X-Backend header, then
// model-family pin, then the mode default. The served model follows the
// pass-through rule (section 6.1): an explicitly named model is served
// unchanged unless one of the four sanctioned rewrites applies — a role
// name, a bare backend name, a model-less request, or a pin mode.
func (c *Config) Resolve(model, header string, mode ModeCfg) (Decision, error) {
	// 1. Role names resolve before everything, as in the prototype:
	//    a role is a deliberate ask for a job, not a model. The serving
	//    backend comes from the mode (its role override, else its
	//    default); the served model is the role's per-backend override
	//    for that backend when one is set, else the role's default.
	if target, isRole := c.Roles[model]; isRole {
		backend := mode.Default
		if override, ok := mode.Roles[model]; ok {
			backend = override
		}
		return Decision{Backend: backend, ServedModel: target.Model(backend), Rewritten: true}, nil
	}

	// 2-4. Backend selection: header, then a bare backend alias as the
	// model name, then family pins, then the mode default.
	backend := ""
	_, isAlias := c.Backends[model]
	if header != "" {
		if _, ok := c.Backends[header]; !ok {
			return Decision{}, fmt.Errorf("unknown backend %q in X-Backend header", header)
		}
		backend = header
	} else if isAlias {
		backend = model
	} else if model != "" {
		for _, name := range c.BackendNames() {
			for _, prefix := range c.Backends[name].ModelPrefixes {
				if strings.HasPrefix(model, prefix) {
					backend = name
					break
				}
			}
			if backend != "" {
				break
			}
		}
	}
	if backend == "" {
		backend = mode.Default
	}

	// Served model: the four sanctioned rewrites, else pass-through.
	b := c.Backends[backend]
	switch {
	case isAlias:
		// Bare backend alias: serve the default model of the backend the
		// alias names (which is the serving backend unless a header
		// overrode it).
		return Decision{Backend: backend, ServedModel: c.Backends[model].DefaultModel, Rewritten: true}, nil
	case model == "":
		return Decision{Backend: backend, ServedModel: b.DefaultModel, Rewritten: true}, nil
	case mode.PinDefaultModel:
		return Decision{Backend: backend, ServedModel: b.DefaultModel, Rewritten: model != b.DefaultModel}, nil
	default:
		return Decision{Backend: backend, ServedModel: model, Rewritten: false}, nil
	}
}
