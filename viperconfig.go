// Package viperconfig provides a generic, tag-based configuration loading and validation system
// built on top of Viper. It supports loading from YAML files and environment variables with
// automatic validation using struct tags.
package viperconfig

import (
"fmt"
"os"
"reflect"
"strconv"
"strings"

"github.com/spf13/viper"
)

// LoadOptions configures how configuration is loaded
type LoadOptions struct {
	envPrefix      string
	configName     string
	configPaths    []string
	configType     string
	noEnvBinding   bool
	strictDefaults bool
}

// Option is a functional option for configuring LoadOptions
type Option func(*LoadOptions)

// WithEnvPrefix sets the environment variable prefix (default: "APP")
// Environment variables will be UPPERCASE(prefix)_KEY_NAME
func WithEnvPrefix(prefix string) Option {
	return func(o *LoadOptions) {
		o.envPrefix = prefix
	}
}

// WithConfigName sets the config file name without extension (default: "config")
func WithConfigName(name string) Option {
	return func(o *LoadOptions) {
		o.configName = name
	}
}

// WithConfigPaths sets the paths to search for config file
// Replaces default paths entirely
func WithConfigPaths(paths ...string) Option {
	return func(o *LoadOptions) {
		o.configPaths = paths
	}
}

// WithConfigType sets the config file type (default: "yaml")
// Supported: yaml, json, toml, etc.
func WithConfigType(configType string) Option {
	return func(o *LoadOptions) {
		o.configType = configType
	}
}

// WithoutEnvKeyBinding disables the per-key env binding that Load performs
// before unmarshaling.
//
// Binding is on by default because without it viper only unmarshals keys that
// already exist in the YAML: an env-only field silently stays at its zero value
// (see the "Quirks" section of the README). Turn it off only to reproduce the
// v1.3.0 behaviour of a service whose environment carries variables that happen
// to collide with config keys it deliberately never wired up.
func WithoutEnvKeyBinding() Option {
	return func(o *LoadOptions) {
		o.noEnvBinding = true
	}
}

// WithStrictDefaults makes an explicitly configured value win over the field's
// `default` tag, including when that value is the type's zero value.
//
// Without it, validate() re-applies the default onto any field isEmpty()
// considers empty, so `false`, `0` and `""` are indistinguishable from "not
// configured" — which is why a bool with `default:"true"` cannot be turned off
// and an int with a non-zero default cannot be set to 0.
//
// It is opt-in in v1.x because turning it on changes the value a service reads
// wherever a config file or env var already sets one of those zero values. It
// becomes the default in v2.0.0. Before enabling it, check what your YAML and
// deployed env actually set: hack/audit-zero-defaults.sh does that for one repo.
func WithStrictDefaults() Option {
	return func(o *LoadOptions) {
		o.strictDefaults = true
	}
}

// defaultOptions returns the default load options
func defaultOptions() LoadOptions {
	return LoadOptions{
		envPrefix:  "APP",
		configName: "config",
		configType: "yaml",
		configPaths: []string{
			".",
			"./config",
			"./configs",
			"/etc/app",
		},
	}
}

// MustLoad loads and validates configuration, panics on error
// This is the simplest API - one line to load everything
//
// Example:
//
// cfg := viperconfig.MustLoad(&MyConfig{})/MyConfig{})
func MustLoad[T any](cfg *T, opts ...Option) *T {
	result, err := Load(cfg, opts...)
	if err != nil {
		panic(err)
	}
	return result
}

// Load loads and validates configuration from YAML file and environment variables
// Returns the populated config struct or an error
//
// Example:
//
// cfg, err := viperconfig.Load(&MyConfig{},/
//     viperconfig.WithEnvPrefix("MYAPP"),
//     viperconfig.WithConfigName("myconfig"),
// )
func Load[T any](cfg *T, opts ...Option) (*T, error) {
	// Apply options
	options := defaultOptions()
	for _, opt := range opts {
		opt(&options)
	}

	// Setup Viper
	v := setupViperWithOptions(options)

	// Create validator and load
	validator := NewValidator(v)
	validator.bindEnvKeys = !options.noEnvBinding
	validator.strictDefaults = options.strictDefaults
	if err := validator.LoadAndValidate(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// setupViperWithOptions configures Viper with the given options
func setupViperWithOptions(opts LoadOptions) *viper.Viper {
	v := viper.New()

	// Config from file depending on env
	if env, present := os.LookupEnv("GO_ENV"); present {
		v.SetConfigName(opts.configName + "." + env)
	} else {
		v.SetConfigName(opts.configName)
	}

	v.SetConfigType(opts.configType)
	for _, path := range opts.configPaths {
		v.AddConfigPath(path)
	}

	err := v.ReadInConfig()
	if err != nil {
		panic(fmt.Errorf("fatal error config file: %w", err))
	}

	// Config from env
	v.SetEnvPrefix(opts.envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	return v
}

// Validator provides configuration validation functionality
type Validator struct {
	v *viper.Viper
	// bindEnvKeys binds every struct key to its env var before unmarshaling.
	// See WithoutEnvKeyBinding.
	bindEnvKeys bool
	// strictDefaults keeps an explicitly configured zero value from being
	// overwritten by a `default` tag. See WithStrictDefaults.
	strictDefaults bool
}

// NewValidator creates a new Validator instance with env-key binding enabled,
// matching what Load does by default.
func NewValidator(v *viper.Viper) *Validator {
	return &Validator{v: v, bindEnvKeys: true}
}

// LoadAndValidate loads configuration from Viper into the provided struct and validates it
// The struct must have mapstructure tags for Viper unmarshaling
//
// Supported tags:
//   - mapstructure:"key_name" - maps to configuration key
//   - optional:"true" - field can be empty (not required)
//   - default:"value" - default value if not set
//   - validate:"rule1,rule2" - validation rules (nonempty, positive, url, hostname)
//
// By default, all fields are required unless marked optional or have a default value.
func (val *Validator) LoadAndValidate(cfg interface{}) error {
	// The struct is the key model: viper only knows the keys its config file
	// happens to declare, so everything below is driven off the fields the
	// caller actually asked to be populated.
	keys := structKeys(cfg)

	// Bind each key to its env var, so an env-only field is unmarshaled at all.
	val.bindEnvKeysToViper(keys)

	// Convert comma-separated strings into slices for []string fields.
	val.convertCSVToSlices(keys)

	// Expand environment variables like ${VAR} or ${VAR:-default}
	val.expandEnvVars()
	
	// First, unmarshal the config
	if err := val.v.Unmarshal(cfg); err != nil {
		return fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Then validate
	return val.validate(reflect.ValueOf(cfg), "", val.v.GetString("envPrefix"))
}

// validate recursively validates a struct based on tags
func (val *Validator) validate(v reflect.Value, prefix string, envPrefix string) error {
	// Handle pointers
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return fmt.Errorf("config pointer is nil")
		}
		v = v.Elem()
	}

	if v.Kind() != reflect.Struct {
		return nil
	}

	t := v.Type()
	
	// Get actual env prefix from viper if not provided
	if envPrefix == "" {
		envPrefix = strings.ToUpper(val.v.GetString("envPrefix"))
		if envPrefix == "" {
			envPrefix = "APP"
		}
	}

	for i := 0; i < v.NumField(); i++ {
		field := v.Field(i)
		fieldType := t.Field(i)

		// Skip unexported fields
		if !field.CanInterface() {
			continue
		}

		// Get mapstructure tag for the key name
		mapstructureTag := fieldType.Tag.Get("mapstructure")
		if mapstructureTag == "" {
			// If no mapstructure tag, use the field name
			mapstructureTag = strings.ToLower(fieldType.Name)
		}

		// Build the full key path
		var fullKey string
		if prefix == "" {
			fullKey = mapstructureTag
		} else {
			fullKey = prefix + "." + mapstructureTag
		}

		// Check if field is a struct and recurse
		if field.Kind() == reflect.Struct {
			if err := val.validate(field, fullKey, envPrefix); err != nil {
				return err
			}
			continue
		}

		// Check if field is a pointer to struct and recurse
		if field.Kind() == reflect.Ptr && !field.IsNil() && field.Elem().Kind() == reflect.Struct {
			if err := val.validate(field.Elem(), fullKey, envPrefix); err != nil {
				return err
			}
			continue
		}

		// Apply default value if specified
		defaultValue := fieldType.Tag.Get("default")
		hasDefault := defaultValue != ""
		if hasDefault && val.isEmpty(field) && !val.explicitlySet(fullKey) {
			if err := val.setDefault(field, defaultValue); err != nil {
				return fmt.Errorf("failed to set default value for %s: %w", fullKey, err)
			}
		}

		// Check if field is optional
		optional := fieldType.Tag.Get("optional") == "true"
		if optional || hasDefault {
			// If field has a default or is optional, skip required validation
			continue
		}

		// By default, fields are required unless marked optional or have default
		if val.isEmpty(field) {
			envVarName := envPrefix + "_" + strings.ToUpper(strings.ReplaceAll(fullKey, ".", "_"))
			return fmt.Errorf("required configuration '%s' is missing or empty. Set it in config file or environment variable %s", fullKey, envVarName)
		}

		// Additional validations
		validateTag := fieldType.Tag.Get("validate")
		if validateTag != "" {
			rules := strings.Split(validateTag, ",")
			for _, rule := range rules {
				rule = strings.TrimSpace(rule)
				if err := val.validateRule(field, rule, fullKey); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// isEmpty checks if a field is empty
func (val *Validator) isEmpty(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.String:
		return v.String() == ""
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Slice, reflect.Map, reflect.Array:
		return v.Len() == 0
	case reflect.Ptr, reflect.Interface:
		return v.IsNil()
	default:
		return false
	}
}

// setDefault sets a default value on a field
func (val *Validator) setDefault(v reflect.Value, defaultValue string) error {
	if !v.CanSet() {
		return fmt.Errorf("cannot set value")
	}

	switch v.Kind() {
	case reflect.String:
		v.SetString(defaultValue)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		intVal, err := strconv.ParseInt(defaultValue, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid default int value '%s': %w", defaultValue, err)
		}
		v.SetInt(intVal)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		uintVal, err := strconv.ParseUint(defaultValue, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid default uint value '%s': %w", defaultValue, err)
		}
		v.SetUint(uintVal)
	case reflect.Float32, reflect.Float64:
		floatVal, err := strconv.ParseFloat(defaultValue, 64)
		if err != nil {
			return fmt.Errorf("invalid default float value '%s': %w", defaultValue, err)
		}
		v.SetFloat(floatVal)
	case reflect.Bool:
		boolVal, err := strconv.ParseBool(defaultValue)
		if err != nil {
			return fmt.Errorf("invalid default bool value '%s': %w", defaultValue, err)
		}
		v.SetBool(boolVal)
	default:
		return fmt.Errorf("unsupported type for default value: %s", v.Kind())
	}

	return nil
}

// validateRule applies a validation rule to a field
func (val *Validator) validateRule(v reflect.Value, rule string, fieldName string) error {
	switch rule {
	case "nonempty":
		if val.isEmpty(v) {
			return fmt.Errorf("field '%s' must not be empty", fieldName)
		}
	case "positive":
		switch v.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			if v.Int() <= 0 {
				return fmt.Errorf("field '%s' must be positive, got %d", fieldName, v.Int())
			}
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			if v.Uint() == 0 {
				return fmt.Errorf("field '%s' must be positive, got %d", fieldName, v.Uint())
			}
		case reflect.Float32, reflect.Float64:
			if v.Float() <= 0 {
				return fmt.Errorf("field '%s' must be positive, got %f", fieldName, v.Float())
			}
		default:
			return fmt.Errorf("positive validation only applies to numeric types")
		}
	case "url":
		if v.Kind() != reflect.String {
			return fmt.Errorf("url validation only applies to strings")
		}
		str := v.String()
		if !strings.HasPrefix(str, "http://") && !strings.HasPrefix(str, "https://") {
			return fmt.Errorf("field '%s' must be a valid URL (http:// or https://)", fieldName)
		}
	case "hostname":
		if v.Kind() != reflect.String {
			return fmt.Errorf("hostname validation only applies to strings")
		}
		str := v.String()
		if str == "" || strings.Contains(str, " ") {
			return fmt.Errorf("field '%s' must be a valid hostname", fieldName)
		}
	default:
		return fmt.Errorf("unknown validation rule: %s", rule)
	}
	return nil
}

// bindEnvKeysToViper binds every key of the config struct to its environment
// variable.
//
// AutomaticEnv alone is not enough: Unmarshal decodes viper's AllSettings, and
// a key that appears in no config file is in no AllSettings, so an env-only
// field is never written and every key-walking step below would skip it too.
// BindEnv registers the key, which puts it in AllKeys and lets the env value
// reach the struct. The env var name is unchanged — it is the same
// prefix + upper(key with "." replaced) that AutomaticEnv looks up.
func (val *Validator) bindEnvKeysToViper(keys []structKey) {
	if !val.bindEnvKeys {
		return
	}
	for _, k := range keys {
		// BindEnv only fails when called with no arguments.
		_ = val.v.BindEnv(k.path)
	}
}

// convertCSVToSlices splits comma-separated strings into slices for every
// []string field, so a list can be supplied by a single env var:
//
//	APP_SERVICE_URLS="url1,url2,url3"
//
// The destination FIELD TYPE decides what gets split, not the key name: the
// pre-1.4 converter only fired on keys ending in "urls" and only on keys viper
// already knew, which silently skipped every env-only list.
func (val *Validator) convertCSVToSlices(keys []structKey) {
	for _, k := range keys {
		if !k.stringSlice {
			continue
		}
		raw, ok := val.v.Get(k.path).(string)
		if !ok || raw == "" {
			continue
		}
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		val.v.Set(k.path, out)
	}
}

// explicitlySet reports whether a config file or env var supplies this key, and
// so whether a `default` tag must keep its hands off the field. Always false
// unless WithStrictDefaults is on, which is what keeps v1.x behaviour intact.
func (val *Validator) explicitlySet(key string) bool {
	return val.strictDefaults && val.v.IsSet(key)
}

// expandEnvVars expands environment variables in config values
// Supports ${VAR} and ${VAR:-default} syntax
func (val *Validator) expandEnvVars() {
	for _, key := range val.v.AllKeys() {
		// For string values
		if strValue := val.v.GetString(key); strValue != "" {
			expanded := os.ExpandEnv(strValue)
			if expanded != strValue {
				val.v.Set(key, expanded)
			}
		}
		
		// For string slices
		if sliceValue := val.v.GetStringSlice(key); len(sliceValue) > 0 {
			expanded := false
			for i, v := range sliceValue {
				expandedV := os.ExpandEnv(v)
				if expandedV != v {
					sliceValue[i] = expandedV
					expanded = true
				}
			}
			if expanded {
				val.v.Set(key, sliceValue)
			}
		}
	}
}
