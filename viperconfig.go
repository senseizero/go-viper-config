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
	envPrefix   string
	configName  string
	configPaths []string
	configType  string
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
/cfg := viperconfig.MustLoad(&MyConfig{})/
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
/cfg, err := viperconfig.Load(&MyConfig{},/
/    viperconfig.WithEnvPrefix("MYAPP"),/
/    viperconfig.WithConfigName("myconfig"),/
/)/
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
}

// NewValidator creates a new Validator instance
func NewValidator(v *viper.Viper) *Validator {
	return &Validator{v: v}
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
	// Convert CSV environment variables to slices for keys ending in "urls"
	val.convertCSVEnvVarsToSlices()
	
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
		if hasDefault && val.isEmpty(field) {
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

// convertCSVEnvVarsToSlices converts comma-separated environment variable strings to slices
// This allows setting list configurations via environment variables like:
// APP_SERVICE_URLS="url1,url2,url3"
func (val *Validator) convertCSVEnvVarsToSlices() {
	for _, key := range val.v.AllKeys() {
		if strings.HasSuffix(key, ".urls") || strings.HasSuffix(key, "urls") {
			value := val.v.GetString(key)
			if value != "" && strings.Contains(value, ",") {
				// Split CSV and trim whitespace
				urls := strings.Split(value, ",")
				for i, url := range urls {
					urls[i] = strings.TrimSpace(url)
				}
				val.v.Set(key, urls)
			}
		}
	}
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
