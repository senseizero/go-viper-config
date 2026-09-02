// Package endpoints turns a configured list of endpoints into the one endpoint
// that answers, without knowing anything about the protocol.
//
// It is the protocol-agnostic sibling of grpcclients.DialWithFallback: the
// caller supplies a probe — an HTTP login, a TCP dial, a REST healthz, a
// database ping — and Select walks the list in order and returns the first
// endpoint whose probe succeeds.
//
// The primary use is ephemeral PR environments. A per-PR namespace only holds
// the services that PR actually changed, so every other dependency has to fall
// back to the long-running dev copy. The deployment expresses that as one
// comma-separated env var, sibling PR first:
//
//	ZERO_ESGCLOUD_URL="http://esg-cloud.esg-cloud-pr-my-branch.svc:7430,http://esg-cloud.dev.svc:7430"
//
// and the service resolves it at construction:
//
//	list := endpoints.Split(cfg.EsgCloud.URL)
//	url, err := endpoints.Select(list, func(ep string) error {
//	        return api.LoginAgainst(ep)
//	})
//	if err != nil {
//	        log.Warn("no endpoint answered, using the first anyway", "err", err)
//	}
//
// Selection happens once, at construction. This is a startup ladder, not a load
// balancer: nothing re-probes later, and nothing spreads traffic. When the
// endpoints are equivalent replicas rather than a priority ladder, use
// grpcclients.DialBalanced instead.
package endpoints

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// ErrNoEndpoints is returned by Select when the list is empty.
var ErrNoEndpoints = errors.New("endpoints: none configured")

// Option configures Select.
type Option func(*options)

type options struct {
	logger      *slog.Logger
	serviceName string
}

func defaults() options {
	return options{logger: slog.Default()}
}

// WithLogger overrides the logger used to report a fallback. Defaults to
// slog.Default(). Passing nil is a no-op.
func WithLogger(l *slog.Logger) Option {
	return func(o *options) {
		if l != nil {
			o.logger = l
		}
	}
}

// WithServiceName tags the log lines so several selections in one process are
// distinguishable in aggregated logs.
func WithServiceName(name string) Option {
	return func(o *options) { o.serviceName = name }
}

// Split turns a comma-separated endpoint string into a list, trimming
// whitespace and dropping empty entries. An empty string yields nil.
//
// It exists because the fleet's convention carries fallback lists in a singular
// string field (ZERO_<SVC>_URL) rather than a list field — the deployed
// manifests set one env var per service and every consumer used to re-implement
// this same three-line split.
func Split(csv string) []string {
	if csv == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Select returns the first endpoint whose probe succeeds.
//
// Three rules make it safe to drop into a service that has always had exactly
// one endpoint:
//
//   - An empty list returns "" and ErrNoEndpoints. That is the only case where
//     the returned endpoint is empty.
//   - A single-endpoint list is returned WITHOUT probing, so a service with one
//     configured URL behaves exactly as it did before failover existed: no
//     extra round trip at boot, and no new way to fail when that URL is merely
//     slow to come up.
//   - When every probe fails, the FIRST endpoint is returned together with the
//     error. The caller gets a usable value whose calls will fail loudly and be
//     retried, instead of a nil client that panics on first use. Callers that
//     would rather give up can just check the error.
//
// A nil probe means there is nothing to select on, so the first endpoint is
// returned unprobed.
func Select(list []string, probe func(string) error, opts ...Option) (string, error) {
	if len(list) == 0 {
		return "", ErrNoEndpoints
	}
	if len(list) == 1 || probe == nil {
		return list[0], nil
	}

	o := defaults()
	for _, opt := range opts {
		opt(&o)
	}

	errs := make([]error, 0, len(list))
	for i, ep := range list {
		err := probe(ep)
		if err == nil {
			if i > 0 {
				o.logger.Info("endpoints: selected a fallback",
					"service", o.serviceName, "endpoint", ep, "position", i+1, "total", len(list))
			}
			return ep, nil
		}
		o.logger.Warn("endpoints: probe failed, trying the next one",
			"service", o.serviceName, "endpoint", ep, "err", err)
		errs = append(errs, fmt.Errorf("%s: %w", ep, err))
	}
	return list[0], fmt.Errorf("endpoints: none of %d answered for %q, falling back to %s: %w",
		len(list), o.serviceName, list[0], errors.Join(errs...))
}
