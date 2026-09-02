# go-viper-config

Generic, tag-based configuration loading and validation system built on Viper.

## Features

- One-line API: `MustLoad(&Config{})`
- Works with any struct
- Tag-based validation
- YAML plus environment variables, env wins
- Lists from a single comma-separated env var
- `${VAR}` expansion
- Endpoint failover for ephemeral PR environments (`endpoints`, `grpcclients`)

## Installation

```bash
go get github.com/senseizero/go-viper-config
```

## Quick Start

```go
package main

import "github.com/senseizero/go-viper-config/viperconfig"

type Config struct {
    Port    int      `mapstructure:"port" default:"8080"`
    Database DBConfig `mapstructure:"database"`
}

type DBConfig struct {
    Host string   `mapstructure:"host"`
    URLs []string `mapstructure:"urls"`
}

func main() {
    // One line loads everything!
    cfg := viperconfig.MustLoad(&Config{})
    
    // Use your config
    println(cfg.Port)
}
```

## Supported Tags

- `mapstructure:"key"` - Config key name
- `optional:"true"` - Field is optional
- `default:"value"` - Default value
- `validate:"positive,url"` - Validation rules

## Environment variables

Priority: env over YAML. The variable name is the prefix plus the dotted key
path, uppercased, dots replaced by underscores.

```bash
export APP_DATABASE_HOST="prod-db.example.com"
export APP_DATABASE_URLS="db1:5432,db2:5432,db3:5432"  # a []string field
```

Since v1.4.0 every key of the config struct is bound to its variable before
unmarshaling, so a field that exists only in the environment is populated even
when no YAML file mentions it. Before that, viper only unmarshaled the keys its
config file declared and env-only fields silently stayed at their zero value.
See [Quirks](#quirks) for what that used to break, and
[Upgrading](#upgrading-from-v130) for what it changes.

A `[]string` field can be filled from one comma-separated variable. Splitting is
decided by the destination field type, so a `string` field keeps its commas:
that is what lets a service carry a fallback list in a singular `url` field and
split it itself.

## Options

```go
cfg := viperconfig.MustLoad(&Config{},
    viperconfig.WithEnvPrefix("MYAPP"),
    viperconfig.WithConfigName("myconfig"),
    viperconfig.WithConfigPaths("./config", "/etc/app"),
)
```

## Endpoint failover

`endpoints` picks the first endpoint that answers, for any protocol. The caller
supplies the probe, so it works for an HTTP login, a REST healthz, a database
ping, or anything else.

```go
import "github.com/senseizero/go-viper-config/endpoints"

// ZERO_ESGCLOUD_URL="http://esg-cloud.esg-cloud-pr-my-branch.svc:7430,http://esg-cloud.dev.svc:7430"
list := endpoints.Split(cfg.EsgCloud.URL)
url, err := endpoints.Select(list, func(ep string) error {
    return api.LoginAgainst(ep)
}, endpoints.WithServiceName("esg-cloud"))
if err != nil {
    // Every endpoint failed. url is still the first one, so the client works
    // once the service recovers; log and carry on rather than returning nil.
    log.Warn("no esg-cloud endpoint answered", "err", err)
}
```

The primary use is the PR-environment pattern: a per-PR namespace only holds the
services that PR changed, so the deployment sets `<my-branch-endpoint>,<dev-endpoint>`
and every consumer lands on its sibling PR copy when it exists and on dev when
it does not.

Three rules make it safe to add to a service that has only ever had one URL:

- One endpoint is returned without probing. No extra round trip at boot, and no
  new way to fail when that single endpoint is just slow to come up.
- An empty list returns `""` and `ErrNoEndpoints`, the only case where the
  returned endpoint is empty.
- When every probe fails, the first endpoint comes back together with the error.
  The caller keeps a usable value whose calls fail loudly and get retried,
  instead of a nil client that panics on first use.

Selection happens once, at construction. This is a startup ladder, not a load
balancer. For equivalent replicas rather than a priority list, use
`grpcclients.DialBalanced`.

## gRPC client fallback

The `grpcclients` subpackage dials a list of gRPC URLs in order and returns
the first connection whose `grpc_health_v1` `Check` succeeds. Fed by a
comma-separated env var, this is how an ephemeral PR environment falls back to
`develop` when a service is not deployed in the PR stack:

```bash
export APP_SUKAUTO_URLS="pr-123-sukauto:9000,develop-sukauto:9000"
```

```go
import (
    "github.com/senseizero/go-viper-config/grpcclients"
    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials/insecure"
)

type Config struct {
    Sukauto struct {
        URLs []string `mapstructure:"urls"`
    } `mapstructure:"sukauto"`
}

cfg := viperconfig.MustLoad(&Config{})

conn, err := grpcclients.DialWithFallback(ctx, cfg.Sukauto.URLs,
    grpcclients.WithServiceName("sukauto"),
    grpcclients.WithDialOptions(grpc.WithTransportCredentials(insecure.NewCredentials())),
)
if err != nil { /* handle */ }
defer conn.Close()

sukauto := proto.NewSukautoClient(conn)
```

Options: `WithDialOptions`, `WithHealthTimeout` (default 5s), `WithServiceName`,
`WithLogger` (`*slog.Logger`, defaults to `slog.Default()`). No async mode —
if you want non-blocking startup, run `DialWithFallback` in a goroutine.

## Quirks

Four behaviours that cost the fleet real debugging time. Each one is verified by
a test in this repo.

### Env-only fields (fixed in v1.4.0)

`Unmarshal` decodes viper's `AllSettings`, which contains the keys the config
file declares. A key no YAML file mentions is not in it, so up to v1.3.0:

- a required field set only in the environment failed validation with
  "required configuration 'x' is missing", however loudly the variable was set;
- the CSV-to-slice conversion skipped it, because that conversion walked
  `v.AllKeys()`;
- and the workaround was to declare every key in the YAML, even empty, purely so
  the env override had something to bind to.

v1.4.0 binds every struct key with `viper.BindEnv` before unmarshaling. The
variable names do not change. `WithoutEnvKeyBinding()` restores the old
behaviour.

The CSV conversion is now driven by the destination field type instead of a key
name ending in `urls`, which is what made env-only lists and any list under a
differently named key come through empty.

### A `default` tag overwrites an explicit zero value

`validate()` runs after `Unmarshal` and re-applies the `default` tag wherever
`isEmpty()` says the field is empty. `isEmpty()` counts `false`, `0`, `0.0` and
`""` as empty, so a configured zero value is indistinguishable from an unset one:

- `bool` with `default:"true"` cannot be turned off. Setting `false` in YAML or
  in the environment has no effect at all, in any environment. This is why
  services carry dead switches, and why some of them phrase flags as
  `disableX bool` with no default tag: a bool whose default is its zero value
  has no tag to re-apply.
- `int` with `default:"3"` cannot be set to `0`.
- `string` with a non-empty default cannot be set to `""`.

`WithStrictDefaults()` makes an explicitly configured value win, zero values
included. It is opt-in in v1.x and becomes the default in v2.0.0, because
turning it on changes what a service reads wherever a config file or deployment
already sets one of those values. Run `hack/audit-zero-defaults.sh` against your
repo first: it lists every such place.

### GO_ENV picks one file, it does not merge

`config.<GO_ENV>.yaml` replaces `config.yaml`, so a key that exists only in
`config.DEVELOPMENT.yaml` does not exist in production. With per-key env binding
this matters less than it did, since an env override no longer needs a YAML key
to land on.

## Upgrading from v1.3.0

v1.4.0 is a minor release: the API is unchanged and no existing call site needs
editing. One behaviour does change on the bump itself.

**Env variables that were inert start working.** Any `<PREFIX>_*` variable your
deployment sets whose key was missing from the YAML did nothing before and takes
effect now. That is normally what whoever set it intended, but it is a live
change, so read the list first:

```bash
hack/audit-inert-env.sh <repo> <deployment-values-dir> ZERO
```

**Nothing else changes unless you ask for it.** `WithStrictDefaults()` is opt-in
precisely so a dependency bump cannot silently flip a switch that has been stuck
at its default. Enable it deliberately, after running:

```bash
hack/audit-zero-defaults.sh <repo> <deployment-values-dir>
```

## License

MIT
