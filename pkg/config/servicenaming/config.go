// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

// Package servicenaming provides configuration loading and validation for service discovery.
package servicenaming

import (
	"fmt"

	"github.com/google/cel-go/cel"

	pkgconfigmodel "github.com/DataDog/datadog-agent/pkg/config/model"
	"github.com/DataDog/datadog-agent/pkg/config/servicenaming/engine"
)

// AgentServiceDiscoveryConfig holds the configuration for CEL-based service discovery in the agent.
type AgentServiceDiscoveryConfig struct {
	Enabled bool `yaml:"enabled"`

	ServiceDefinitions []ServiceDefinition `yaml:"service_definitions"`
}

// ServiceDefinition represents a query/value pair for service name evaluation.
type ServiceDefinition struct {
	// Name is an optional identifier
	Name string `yaml:"name,omitempty"`

	Query string `yaml:"query"`

	Value string `yaml:"value"`
}

// configReader is a minimal interface for reading service discovery config.
// This allows for easier testing without mocking the full pkgconfigmodel.Reader.
type configReader interface {
	GetBool(key string) bool
	Get(key string) interface{}
}

// LoadFromAgentConfig loads the service discovery configuration from the given config reader.
func LoadFromAgentConfig(cfg pkgconfigmodel.Reader) (*AgentServiceDiscoveryConfig, error) {
	return loadFromReader(cfg)
}

// loadFromReader is the internal implementation of configuration loading.
func loadFromReader(cfg configReader) (*AgentServiceDiscoveryConfig, error) {
	config := &AgentServiceDiscoveryConfig{
		Enabled: cfg.GetBool("service_discovery.enabled"),
	}

	// Get service definitions from config
	// The config system returns []interface{} for slices
	rawDefs := cfg.Get("service_discovery.service_definitions")
	if rawDefs != nil {
		defs, err := parseServiceDefinitions(rawDefs)
		if err != nil {
			return nil, fmt.Errorf("invalid service_definitions: %w", err)
		}
		config.ServiceDefinitions = defs
	}

	// If not enabled, we can skip validation (allows partial/invalid config when disabled)
	if !config.Enabled {
		return config, nil
	}

	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return config, nil
}

// parseServiceDefinitions parses the raw service definitions from datadog.yaml into structured ServiceDefinition objects.
// This function expects the YAML parser format: []interface{} containing map[interface{}]interface{} items.
func parseServiceDefinitions(raw interface{}) ([]ServiceDefinition, error) {
	// YAML parser produces []interface{}
	slice, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("expected array, got %T", raw)
	}

	defs := make([]ServiceDefinition, 0, len(slice))
	for i, item := range slice {
		// YAML parser produces map[interface{}]interface{} for each item
		m, ok := item.(map[interface{}]interface{})
		if !ok {
			return nil, fmt.Errorf("service_definitions[%d]: expected map, got %T", i, item)
		}

		def := ServiceDefinition{}

		// Extract name (optional)
		if nameVal, exists := m["name"]; exists {
			if name, ok := nameVal.(string); ok {
				def.Name = name
			}
		}

		// Query is required - check type and value separately for better error messages
		queryVal, queryExists := m["query"]
		if !queryExists {
			return nil, fmt.Errorf("service_definition[%d]: missing required field 'query'", i)
		}
		query, ok := queryVal.(string)
		if !ok {
			return nil, fmt.Errorf("service_definition[%d]: query must be a string, got %T", i, queryVal)
		}
		if query == "" {
			return nil, fmt.Errorf("service_definition[%d]: query cannot be empty", i)
		}
		def.Query = query

		// Value is required - check type and value separately for better error messages
		valueVal, valueExists := m["value"]
		if !valueExists {
			return nil, fmt.Errorf("service_definition[%d]: missing required field 'value'", i)
		}
		value, ok := valueVal.(string)
		if !ok {
			return nil, fmt.Errorf("service_definition[%d]: value must be a string, got %T", i, valueVal)
		}
		if value == "" {
			return nil, fmt.Errorf("service_definition[%d]: value cannot be empty", i)
		}
		def.Value = value

		defs = append(defs, def)
	}

	return defs, nil
}

// IsActive returns if service discovery is enabled and has at least one rule defined.
func (c *AgentServiceDiscoveryConfig) IsActive() bool {
	return c.Enabled && len(c.ServiceDefinitions) > 0
}

// Validate checks that all service definitions have valid CEL expressions.
// Empty query/value fields and CEL compilation errors are reported.
func (c *AgentServiceDiscoveryConfig) Validate() error {
	if len(c.ServiceDefinitions) == 0 {
		return nil // Empty config is valid (disabled)
	}

	for i, def := range c.ServiceDefinitions {
		if def.Query == "" {
			return fmt.Errorf("service_definition[%d]: query cannot be empty", i)
		}
		if def.Value == "" {
			return fmt.Errorf("service_definition[%d]: value cannot be empty", i)
		}

		// Validate query compiles as boolean
		if err := validateCELBooleanExpression(def.Query); err != nil {
			return fmt.Errorf("service_definition[%d]: invalid query: %w", i, err)
		}

		// Validate value compiles as string
		if err := validateCELStringExpression(def.Value); err != nil {
			return fmt.Errorf("service_definition[%d]: invalid value: %w", i, err)
		}
	}

	return nil
}

// createCELEnvironmentForValidation creates a CEL environment for validation.
func createCELEnvironmentForValidation() (*cel.Env, error) {
	return engine.CreateCELEnvironment()
}

// validateCELBooleanExpression validates that an expression compiles and returns boolean.
func validateCELBooleanExpression(expr string) error {
	env, err := createCELEnvironmentForValidation()
	if err != nil {
		return err
	}

	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return fmt.Errorf("compilation error: %w", issues.Err())
	}

	// Accept BoolType or DynType (runtime validation will ensure it's actually bool)
	outType := ast.OutputType()
	if outType != cel.BoolType && outType != cel.DynType {
		return fmt.Errorf("expression must return boolean, got %v", outType)
	}

	return nil
}

// validateCELStringExpression validates that an expression compiles and returns string.
func validateCELStringExpression(expr string) error {
	env, err := createCELEnvironmentForValidation()
	if err != nil {
		return err
	}

	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return fmt.Errorf("compilation error: %w", issues.Err())
	}

	// Accept StringType or DynType (runtime validation will ensure it's actually string)
	outType := ast.OutputType()
	if outType != cel.StringType && outType != cel.DynType {
		return fmt.Errorf("expression must return string, got %v", outType)
	}

	return nil
}

// CompileEngine compiles the service definitions into an executable engine.Engine instance.
func (c *AgentServiceDiscoveryConfig) CompileEngine() (*engine.Engine, error) {
	if !c.IsActive() {
		return nil, nil
	}

	// Convert ServiceDefinitions to engine Rules
	rules := make([]engine.Rule, len(c.ServiceDefinitions))
	for i, def := range c.ServiceDefinitions {
		rules[i] = engine.Rule{
			Name:  def.Name,
			Query: def.Query,
			Value: def.Value,
		}
	}

	return engine.NewEngine(rules)
}
