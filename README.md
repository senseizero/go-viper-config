# go-viper-config

Generic, tag-based configuration loading and validation system built on Viper.

## Features

✅ **One-line API**: `MustLoad(&Config{})`  
✅ **100% Generic**: Works with any struct  
✅ **Auto Validation**: Tag-based validation  
✅ **YAML + ENV**: Automatic priority (ENV > YAML)  
✅ **CSV Support**: Lists from environment variables  
✅ **Variable Expansion**: `${VAR}` support  

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

## Environment Variables

Priority: **ENV > YAML**

```bash
export APP_DATABASE_HOST="prod-db.example.com"
export APP_DATABASE_URLS="db1:5432,db2:5432,db3:5432"  # CSV support
```

## Options

```go
cfg := viperconfig.MustLoad(&Config{},
    viperconfig.WithEnvPrefix("MYAPP"),
    viperconfig.WithConfigName("myconfig"),
    viperconfig.WithConfigPaths("./config", "/etc/app"),
)
```

## gRPC client fallback

The `grpcclients` subpackage dials a list of gRPC URLs in order and returns
the first connection whose `grpc_health_v1` `Check` succeeds. Paired with the
CSV-to-slice env handling for `*urls` keys, this makes ephemeral PR
environments trivially fall back to `develop` when a service isn't deployed
in the PR stack:

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

## License

MIT
