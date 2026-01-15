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
go get forgejo.senseivision.ai/senseivision/go-viper-config
```

## Quick Start

```go
package main

import "forgejo.senseivision.ai/senseivision/go-viper-config/viperconfig"

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

## License

MIT
