package config

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"time"

	yaml "gopkg.in/yaml.v3"
)

// EmbeddedDefaultConfig contains the default configuration YAML that gets compiled into the binary.
// This eliminates the need for external config files when installing via nix build.
const EmbeddedDefaultConfig = `default:
	ai_provider: ollama  # Options: openai, bedrock, ollama, gemini, custom
    ai_model: llama3
    # Custom AI provider configuration (used if ai_provider: custom)
    custom_ai:
        base_url: http://localhost:8080/api/generate  # HTTP API endpoint URL
        headers:  # Optional custom headers (e.g., for authentication)
            Authorization: "Bearer your-api-key-here"
            Content-Type: "application/json"  # Set automatically if not provided
    # Basic AI models configuration (subset for embedded config)
    ai_models:
        providers:
            ollama:
                name: "Ollama"
                description: "Local AI model provider"
                type: "local"
                base_url: "http://localhost:11434"
                available: true
                supports_streaming: true
                supports_tools: true
                requires_api_key: false
                models:
                    llama3:
                        name: "Llama 3"
                        description: "Meta's Llama 3 model"
                        type: "chat"
                        context_window: 8192
                        max_tokens: 4096
                        recommended_for: ["nixos", "general"]
                        default: true
			bedrock:
				name: "AWS Bedrock"
				description: "AWS Bedrock OpenAI-compatible endpoint"
				type: "cloud"
				base_url: "https://bedrock-mantle.eu-north-1.api.aws/v1"
				available: true
				supports_streaming: true
				supports_tools: true
				requires_api_key: true
				env_var: "AWS_BEDROCK_API_KEY"
				models:
					us.anthropic.claude-haiku-4-5-20251001-v1:0:
						name: "Claude Haiku 4.5 (Bedrock)"
						description: "Anthropic Claude Haiku 4.5 via AWS Bedrock"
						type: "chat"
						context_window: 200000
						max_tokens: 8192
						recommended_for: ["fast", "general"]
						default: true
					openai.gpt-oss-120b-1:0:
						name: "GPT OSS 120B (Bedrock)"
						description: "OpenAI GPT OSS 120B via AWS Bedrock"
						type: "chat"
						context_window: 128000
						max_tokens: 8192
						recommended_for: ["analysis", "complex"]
        selection_preferences:
            default_provider: "ollama"
            default_models:
                ollama: "llama3"
            task_models:
                nixos_config:
                    primary: ["ollama:llama3"]
                    fallback: []
        discovery:
            auto_discover: true
            cache_duration: 3600
            check_timeout: 10
            max_retries: 2
    log_level: info
    mcp_server:
        host: localhost
        port: 8081
        mcp_port: 39847
        socket_path: /tmp/nixai-mcp.sock
        auto_start: false
        documentation_sources:
            - https://wiki.nixos.org/wiki/NixOS_Wiki
            - https://nix.dev/manual/nix
            - https://nix.dev/ 
            - https://nixos.org/manual/nixpkgs/stable/
            - https://nix.dev/manual/nix/2.28/language/
            - https://nix-community.github.io/home-manager/
    nixos:
        config_path: /etc/nixos/configuration.nix
        log_path: /var/log/nixos.log
    diagnostics:
        enabled: true
        threshold: 5
        error_patterns:
            - name: example_error
              pattern: '(?i)example error regex'
              error_type: custom
              severity: high
              description: Example error description
    commands:
        timeout: 30
        retries: 3
	ai_timeouts:
		ollama: 60
		llamacpp: 120
		gemini: 30
		openai: 30
		bedrock: 30
		custom: 60
		default: 60
    devenv:
        default_directory: "."
        auto_init_git: true
        templates:
            python:
                enabled: true
    execution:
        enabled: true
        confirmation_required: true
        dry_run_default: false
        max_execution_time: 10m
        default_working_dir: ""
        allowed_commands:
            - "nix*"
            - "nixos-*"
            - "systemctl"
            - "journalctl"
            - "ls"
            - "cat"
            - "grep"
            - "find"
            - "git"
        forbidden_commands:
            - "rm -rf /"
            - "dd"
            - "mkfs*"
            - "fdisk"
        sudo_commands:
            - "nixos-rebuild"
            - "systemctl"
        allowed_directories:
            - "/home"
            - "/etc/nixos"
            - "/tmp"
            - "/var/log"
        forbidden_paths:
            - "/boot"
            - "/sys"
            - "/proc"
        allowed_environment_variables:
            - "PATH"
            - "HOME"
            - "NIX_PATH"
        categories:
            package:
                commands: ["nix", "nix-env", "nix-shell", "nix-build"]
                requires_confirmation: false
                requires_sudo: false
                max_execution_time: 5m
            system:
                commands: ["nixos-rebuild", "systemctl"]
                requires_confirmation: true
                requires_sudo: true
                max_execution_time: 15m
        security:
            audit_logging: true
            audit_log_path: "/var/log/nixai/commands.log"
            session_timeout: 30m
            password_timeout: 15m
            max_concurrent_commands: 3
    discourse:
        base_url: "https://discourse.nixos.org"
        api_key: ""  # Optional: set via DISCOURSE_API_KEY environment variable
        username: ""  # Optional: set via DISCOURSE_USERNAME environment variable
        enabled: true
    cache:
        enabled: true
        memory_max_size: 1000        # Max entries in memory
        memory_ttl: 30               # Memory cache TTL in minutes
        disk_enabled: true           # Enable persistent disk cache
        disk_path: ""                # Path for disk cache (will be set to user cache dir)
        disk_max_size: 100           # Max disk cache size in MB
        disk_ttl: 24                 # Disk cache TTL in hours
        cleanup_interval: 5          # Cleanup interval in minutes
        compact_interval: 60         # Compaction interval in minutes
`

type Config struct {
	AIProvider string `json:"ai_provider"`
	MCPServer  string `json:"mcp_server"`
	LogLevel   string `json:"log_level"`
	// Add other configuration fields as needed
}

type MCPServerConfig struct {
	Host                 string   `yaml:"host" json:"host"`
	Port                 int      `yaml:"port" json:"port"`
	MCPPort              int      `yaml:"mcp_port" json:"mcp_port"`
	SocketPath           string   `yaml:"socket_path" json:"socket_path"`
	AutoStart            bool     `yaml:"auto_start" json:"auto_start"`
	DocumentationSources []string `yaml:"documentation_sources" json:"documentation_sources"`
}

type NixosConfig struct {
	ConfigPath string `yaml:"config_path" json:"config_path"`
	LogPath    string `yaml:"log_path" json:"log_path"`
}

// NixOSContext represents the detected NixOS configuration context
type NixOSContext struct {
	// System Detection
	UsesFlakes      bool   `yaml:"uses_flakes" json:"uses_flakes"`
	UsesChannels    bool   `yaml:"uses_channels" json:"uses_channels"`
	NixOSConfigPath string `yaml:"nixos_config_path" json:"nixos_config_path"`
	SystemType      string `yaml:"system_type" json:"system_type"` // "nixos", "nix-darwin", "home-manager-only", "unknown"

	// Home Manager
	HasHomeManager        bool   `yaml:"has_home_manager" json:"has_home_manager"`
	HomeManagerType       string `yaml:"home_manager_type" json:"home_manager_type"` // "standalone", "module", "none"
	HomeManagerConfigPath string `yaml:"home_manager_config_path" json:"home_manager_config_path"`

	// Version Information
	NixOSVersion string `yaml:"nixos_version" json:"nixos_version"`
	NixVersion   string `yaml:"nix_version" json:"nix_version"`

	// Configuration Analysis
	ConfigurationFiles []string `yaml:"configuration_files" json:"configuration_files"`
	EnabledServices    []string `yaml:"enabled_services" json:"enabled_services"`
	InstalledPackages  []string `yaml:"installed_packages" json:"installed_packages"`

	// File Paths
	FlakeFile         string `yaml:"flake_file" json:"flake_file"`
	ConfigurationNix  string `yaml:"configuration_nix" json:"configuration_nix"`
	HardwareConfigNix string `yaml:"hardware_config_nix" json:"hardware_config_nix"`

	// System Information (newly added)
	HardwareInfo    *HardwareInfo    `yaml:"hardware_info,omitempty" json:"hardware_info,omitempty"`
	NetworkInfo     *NetworkInfo     `yaml:"network_info,omitempty" json:"network_info,omitempty"`
	SecurityInfo    *SecurityInfo    `yaml:"security_info,omitempty" json:"security_info,omitempty"`
	PerformanceInfo *PerformanceInfo `yaml:"performance_info,omitempty" json:"performance_info,omitempty"`
	UserEnvironment *UserEnvironment `yaml:"user_environment,omitempty" json:"user_environment,omitempty"`

	// Cache Information
	LastDetected    time.Time `yaml:"last_detected" json:"last_detected"`
	CacheExpires    time.Time `yaml:"cache_expires" json:"cache_expires"`
	CacheValid      bool      `yaml:"cache_valid" json:"cache_valid"`
	DetectionErrors []string  `yaml:"detection_errors,omitempty" json:"detection_errors,omitempty"`
}

// ErrorPatternConfig allows user-defined error patterns for diagnostics
// Pattern is a regex string
// Example YAML:
//   - name: my_error
//     pattern: '(?i)my error regex'
//     error_type: custom
//     severity: high
//     description: My custom error

type ErrorPatternConfig struct {
	Name        string `yaml:"name" json:"name"`
	Pattern     string `yaml:"pattern" json:"pattern"`
	ErrorType   string `yaml:"error_type" json:"error_type"`
	Severity    string `yaml:"severity" json:"severity"`
	Description string `yaml:"description" json:"description"`
}

type DiagnosticsConfig struct {
	Enabled       bool                 `yaml:"enabled" json:"enabled"`
	Threshold     int                  `yaml:"threshold" json:"threshold"`
	ErrorPatterns []ErrorPatternConfig `yaml:"error_patterns" json:"error_patterns"`
}

type CommandsConfig struct {
	Timeout int `yaml:"timeout" json:"timeout"`
	Retries int `yaml:"retries"`
}

// AITimeoutsConfig represents timeout settings for AI providers
type AITimeoutsConfig struct {
	Ollama   int `yaml:"ollama" json:"ollama"`
	LlamaCpp int `yaml:"llamacpp" json:"llamacpp"`
	Gemini   int `yaml:"gemini" json:"gemini"`
	OpenAI   int `yaml:"openai" json:"openai"`
	Bedrock  int `yaml:"bedrock" json:"bedrock"`
	Custom   int `yaml:"custom" json:"custom"`
	Default  int `yaml:"default" json:"default"`
}

type DevenvTemplateConfig struct {
	Enabled               bool   `yaml:"enabled" json:"enabled"`
	DefaultVersion        string `yaml:"default_version" json:"default_version"`
	DefaultPackageManager string `yaml:"default_package_manager"`
}

type DevenvConfig struct {
	DefaultDirectory string                          `yaml:"default_directory" json:"default_directory"`
	AutoInitGit      bool                            `yaml:"auto_init_git" json:"auto_init_git"`
	Templates        map[string]DevenvTemplateConfig `yaml:"templates" json:"templates"`
}

// CustomAIConfig holds config for a custom AI provider
type CustomAIConfig struct {
	BaseURL string            `yaml:"base_url" json:"base_url"`
	Headers map[string]string `yaml:"headers" json:"headers"`
}

// DiscourseConfig holds configuration for Discourse integration
type DiscourseConfig struct {
	BaseURL  string `yaml:"base_url" json:"base_url"`
	APIKey   string `yaml:"api_key" json:"api_key"`
	Username string `yaml:"username" json:"username"`
	Enabled  bool   `yaml:"enabled" json:"enabled"`
}

// CacheConfig represents cache configuration settings
type CacheConfig struct {
	Enabled         bool   `yaml:"enabled" json:"enabled"`                   // Enable/disable caching
	MemoryMaxSize   int    `yaml:"memory_max_size" json:"memory_max_size"`   // Max entries in memory
	MemoryTTL       int    `yaml:"memory_ttl" json:"memory_ttl"`             // Memory cache TTL in minutes
	DiskEnabled     bool   `yaml:"disk_enabled" json:"disk_enabled"`         // Enable persistent disk cache
	DiskPath        string `yaml:"disk_path" json:"disk_path"`               // Path for disk cache
	DiskMaxSize     int64  `yaml:"disk_max_size" json:"disk_max_size"`       // Max disk cache size in MB
	DiskTTL         int    `yaml:"disk_ttl" json:"disk_ttl"`                 // Disk cache TTL in hours
	CleanupInterval int    `yaml:"cleanup_interval" json:"cleanup_interval"` // Cleanup interval in minutes
	CompactInterval int    `yaml:"compact_interval" json:"compact_interval"` // Compaction interval in minutes
}

// PluginConfig represents the plugin system configuration
type PluginConfig struct {
	Enabled         bool                     `yaml:"enabled" json:"enabled"`
	Directory       string                   `yaml:"directory" json:"directory"`
	CacheDirectory  string                   `yaml:"cache_directory" json:"cache_directory"`
	ConfigDirectory string                   `yaml:"config_directory" json:"config_directory"`
	AutoDiscover    bool                     `yaml:"auto_discover" json:"auto_discover"`
	AutoUpdate      bool                     `yaml:"auto_update" json:"auto_update"`
	SandboxEnabled  bool                     `yaml:"sandbox_enabled" json:"sandbox_enabled"`
	MaxConcurrent   int                      `yaml:"max_concurrent" json:"max_concurrent"`
	Timeout         int                      `yaml:"timeout" json:"timeout"`
	Repositories    []PluginRepositoryConfig `yaml:"repositories" json:"repositories"`
	Marketplace     PluginMarketplaceConfig  `yaml:"marketplace" json:"marketplace"`
	Security        PluginSecurityConfig     `yaml:"security" json:"security"`
	PackageManager  PluginPackageConfig      `yaml:"package_manager" json:"package_manager"`
}

// PluginRepositoryConfig represents a plugin repository configuration
type PluginRepositoryConfig struct {
	Name     string `yaml:"name" json:"name"`
	URL      string `yaml:"url" json:"url"`
	Type     string `yaml:"type" json:"type"`
	Enabled  bool   `yaml:"enabled" json:"enabled"`
	Priority int    `yaml:"priority" json:"priority"`
	Verified bool   `yaml:"verified" json:"verified"`
}

// PluginMarketplaceConfig represents marketplace configuration
type PluginMarketplaceConfig struct {
	Enabled         bool   `yaml:"enabled" json:"enabled"`
	BaseURL         string `yaml:"base_url" json:"base_url"`
	CacheDuration   int    `yaml:"cache_duration" json:"cache_duration"`
	FeaturedPlugins int    `yaml:"featured_plugins" json:"featured_plugins"`
	SearchTimeout   int    `yaml:"search_timeout" json:"search_timeout"`
}

// ExecutionConfig represents command execution configuration
type ExecutionConfig struct {
	Enabled                     bool                               `yaml:"enabled" json:"enabled"`
	ConfirmationRequired        bool                               `yaml:"confirmation_required" json:"confirmation_required"`
	DryRunDefault               bool                               `yaml:"dry_run_default" json:"dry_run_default"`
	MaxExecutionTime            time.Duration                      `yaml:"max_execution_time" json:"max_execution_time"`
	DefaultWorkingDir           string                             `yaml:"default_working_dir" json:"default_working_dir"`
	AllowedCommands             []string                           `yaml:"allowed_commands" json:"allowed_commands"`
	ForbiddenCommands           []string                           `yaml:"forbidden_commands" json:"forbidden_commands"`
	SudoCommands                []string                           `yaml:"sudo_commands" json:"sudo_commands"`
	AllowedDirectories          []string                           `yaml:"allowed_directories" json:"allowed_directories"`
	ForbiddenPaths              []string                           `yaml:"forbidden_paths" json:"forbidden_paths"`
	AllowedEnvironmentVariables []string                           `yaml:"allowed_environment_variables" json:"allowed_environment_variables"`
	Categories                  map[string]ExecutionCategoryConfig `yaml:"categories" json:"categories"`
	Security                    ExecutionSecurityConfig            `yaml:"security" json:"security"`
}

// ExecutionCategoryConfig represents configuration for a command category
type ExecutionCategoryConfig struct {
	Commands             []string      `yaml:"commands" json:"commands"`
	RequiresConfirmation bool          `yaml:"requires_confirmation" json:"requires_confirmation"`
	RequiresSudo         bool          `yaml:"requires_sudo" json:"requires_sudo"`
	MaxExecutionTime     time.Duration `yaml:"max_execution_time" json:"max_execution_time"`
	AllowedDirectories   []string      `yaml:"allowed_directories" json:"allowed_directories"`
	ForbiddenArgs        []string      `yaml:"forbidden_args" json:"forbidden_args"`
	AllowedArgs          []string      `yaml:"allowed_args" json:"allowed_args"`
}

// ExecutionSecurityConfig represents security configuration for command execution
type ExecutionSecurityConfig struct {
	AuditLogging          bool          `yaml:"audit_logging" json:"audit_logging"`
	AuditLogPath          string        `yaml:"audit_log_path" json:"audit_log_path"`
	SessionTimeout        time.Duration `yaml:"session_timeout" json:"session_timeout"`
	PasswordTimeout       time.Duration `yaml:"password_timeout" json:"password_timeout"`
	MaxConcurrentCommands int           `yaml:"max_concurrent_commands" json:"max_concurrent_commands"`
	RequireSudoFor        []string      `yaml:"require_sudo_for" json:"require_sudo_for"`
}

// PluginSecurityConfig represents security configuration for plugins
type PluginSecurityConfig struct {
	SandboxEnabled       bool     `yaml:"sandbox_enabled" json:"sandbox_enabled"`
	AllowNetwork         bool     `yaml:"allow_network" json:"allow_network"`
	AllowFilesystemWrite bool     `yaml:"allow_filesystem_write" json:"allow_filesystem_write"`
	AllowSystemCalls     bool     `yaml:"allow_system_calls" json:"allow_system_calls"`
	MaxMemoryMB          int      `yaml:"max_memory_mb" json:"max_memory_mb"`
	MaxCPUPercent        int      `yaml:"max_cpu_percent" json:"max_cpu_percent"`
	AllowedDomains       []string `yaml:"allowed_domains" json:"allowed_domains"`
	BlockedCapabilities  []string `yaml:"blocked_capabilities" json:"blocked_capabilities"`
}

// PluginPackageConfig represents package manager configuration
type PluginPackageConfig struct {
	VerifySignatures    bool `yaml:"verify_signatures" json:"verify_signatures"`
	AllowUnsigned       bool `yaml:"allow_unsigned" json:"allow_unsigned"`
	UpdateCheckInterval int  `yaml:"update_check_interval" json:"update_check_interval"`
	DownloadTimeout     int  `yaml:"download_timeout" json:"download_timeout"`
	MaxDownloadSizeMB   int  `yaml:"max_download_size_mb" json:"max_download_size_mb"`
}

// AI Models Configuration Structures

// AIModelConfig represents a single AI model configuration
type AIModelConfig struct {
	Name             string   `yaml:"name" json:"name"`
	Description      string   `yaml:"description" json:"description"`
	Size             string   `yaml:"size,omitempty" json:"size,omitempty"`
	Type             string   `yaml:"type" json:"type"` // chat, code, completion
	ContextWindow    int      `yaml:"context_window" json:"context_window"`
	MaxTokens        int      `yaml:"max_tokens" json:"max_tokens"`
	RecommendedFor   []string `yaml:"recommended_for" json:"recommended_for"`
	RequiresDownload bool     `yaml:"requires_download,omitempty" json:"requires_download,omitempty"`
	CostTier         string   `yaml:"cost_tier,omitempty" json:"cost_tier,omitempty"` // budget, standard, premium
	Default          bool     `yaml:"default,omitempty" json:"default,omitempty"`
}

// AIProviderConfig represents a single AI provider configuration
type AIProviderConfig struct {
	Name              string                   `yaml:"name" json:"name"`
	Description       string                   `yaml:"description" json:"description"`
	Type              string                   `yaml:"type" json:"type"` // local, cloud, custom
	BaseURL           string                   `yaml:"base_url" json:"base_url"`
	Available         bool                     `yaml:"available" json:"available"`
	SupportsStreaming bool                     `yaml:"supports_streaming" json:"supports_streaming"`
	SupportsTools     bool                     `yaml:"supports_tools" json:"supports_tools"`
	RequiresAPIKey    bool                     `yaml:"requires_api_key" json:"requires_api_key"`
	EnvVar            string                   `yaml:"env_var,omitempty" json:"env_var,omitempty"`
	Models            map[string]AIModelConfig `yaml:"models" json:"models"`
}

// TaskModelPreferences represents model preferences for specific tasks
type TaskModelPreferences struct {
	Primary  []string `yaml:"primary" json:"primary"`
	Fallback []string `yaml:"fallback" json:"fallback"`
}

// AISelectionPreferences represents model selection preferences
type AISelectionPreferences struct {
	DefaultProvider string                          `yaml:"default_provider" json:"default_provider"`
	DefaultModels   map[string]string               `yaml:"default_models" json:"default_models"`
	TaskModels      map[string]TaskModelPreferences `yaml:"task_models" json:"task_models"`
}

// AIDiscoveryConfig represents model discovery configuration
type AIDiscoveryConfig struct {
	AutoDiscover  bool `yaml:"auto_discover" json:"auto_discover"`
	CacheDuration int  `yaml:"cache_duration" json:"cache_duration"`
	CheckTimeout  int  `yaml:"check_timeout" json:"check_timeout"`
	MaxRetries    int  `yaml:"max_retries" json:"max_retries"`
}

// AIModelsConfig represents the complete AI models configuration
type AIModelsConfig struct {
	Providers            map[string]AIProviderConfig `yaml:"providers" json:"providers"`
	SelectionPreferences AISelectionPreferences      `yaml:"selection_preferences" json:"selection_preferences"`
	Discovery            AIDiscoveryConfig           `yaml:"discovery" json:"discovery"`
}

type YAMLConfig struct {
	AIProvider  string            `yaml:"ai_provider" json:"ai_provider"`
	LogLevel    string            `yaml:"log_level" json:"log_level"`
	AIModels    AIModelsConfig    `yaml:"ai_models" json:"ai_models"`
	MCPServer   MCPServerConfig   `yaml:"mcp_server" json:"mcp_server"`
	Nixos       NixosConfig       `yaml:"nixos" json:"nixos"`
	Diagnostics DiagnosticsConfig `yaml:"diagnostics" json:"diagnostics"`
	Commands    CommandsConfig    `yaml:"commands" json:"commands"`
	AITimeouts  AITimeoutsConfig  `yaml:"ai_timeouts" json:"ai_timeouts"`
	Devenv      DevenvConfig      `yaml:"devenv" json:"devenv"`
	CustomAI    CustomAIConfig    `yaml:"custom_ai" json:"custom_ai"`
	Discourse   DiscourseConfig   `yaml:"discourse" json:"discourse"`
	Cache       CacheConfig       `yaml:"cache" json:"cache"`
	Plugin      PluginConfig      `yaml:"plugin" json:"plugin"`
	Execution   ExecutionConfig   `yaml:"execution" json:"execution"`
}

type UserConfig struct {
	AIProvider   string            `yaml:"ai_provider" json:"ai_provider"`
	AIModel      string            `yaml:"ai_model" json:"ai_model"`
	NixosFolder  string            `yaml:"nixos_folder" json:"nixos_folder"`
	LogLevel     string            `yaml:"log_level" json:"log_level"`
	AIModels     AIModelsConfig    `yaml:"ai_models" json:"ai_models"`
	MCPServer    MCPServerConfig   `yaml:"mcp_server" json:"mcp_server"`
	Nixos        NixosConfig       `yaml:"nixos" json:"nixos"`
	Diagnostics  DiagnosticsConfig `yaml:"diagnostics" json:"diagnostics"`
	Commands     CommandsConfig    `yaml:"commands" json:"commands"`
	AITimeouts   AITimeoutsConfig  `yaml:"ai_timeouts" json:"ai_timeouts"`
	Devenv       DevenvConfig      `yaml:"devenv" json:"devenv"`
	CustomAI     CustomAIConfig    `yaml:"custom_ai" json:"custom_ai"`
	Discourse    DiscourseConfig   `yaml:"discourse" json:"discourse"`
	NixOSContext NixOSContext      `yaml:"nixos_context" json:"nixos_context"`
	Cache        CacheConfig       `yaml:"cache" json:"cache"`
	Plugin       PluginConfig      `yaml:"plugin" json:"plugin"`
	Execution    ExecutionConfig   `yaml:"execution" json:"execution"`
}

// GetAITimeout returns the timeout for a specific AI provider
func (c *UserConfig) GetAITimeout(provider string) time.Duration {
	var timeoutSeconds int

	switch provider {
	case "ollama":
		timeoutSeconds = c.AITimeouts.Ollama
	case "llamacpp":
		timeoutSeconds = c.AITimeouts.LlamaCpp
	case "gemini":
		timeoutSeconds = c.AITimeouts.Gemini
	case "openai":
		timeoutSeconds = c.AITimeouts.OpenAI
	case "bedrock":
		timeoutSeconds = c.AITimeouts.Bedrock
	case "custom":
		timeoutSeconds = c.AITimeouts.Custom
	default:
		timeoutSeconds = c.AITimeouts.Default
	}

	// If timeout is 0 or negative, use default
	if timeoutSeconds <= 0 {
		timeoutSeconds = c.AITimeouts.Default
		if timeoutSeconds <= 0 {
			timeoutSeconds = 60 // hardcoded fallback
		}
	}

	return time.Duration(timeoutSeconds) * time.Second
}

// GetAITimeout returns the timeout for a specific AI provider from YAMLConfig
func (c *YAMLConfig) GetAITimeout(provider string) time.Duration {
	var timeoutSeconds int

	switch provider {
	case "ollama":
		timeoutSeconds = c.AITimeouts.Ollama
	case "llamacpp":
		timeoutSeconds = c.AITimeouts.LlamaCpp
	case "gemini":
		timeoutSeconds = c.AITimeouts.Gemini
	case "openai":
		timeoutSeconds = c.AITimeouts.OpenAI
	case "bedrock":
		timeoutSeconds = c.AITimeouts.Bedrock
	case "custom":
		timeoutSeconds = c.AITimeouts.Custom
	default:
		timeoutSeconds = c.AITimeouts.Default
	}

	// If timeout is 0 or negative, use default
	if timeoutSeconds <= 0 {
		timeoutSeconds = c.AITimeouts.Default
		if timeoutSeconds <= 0 {
			timeoutSeconds = 60 // hardcoded fallback
		}
	}

	return time.Duration(timeoutSeconds) * time.Second
}

func DefaultUserConfig() *UserConfig {
	return &UserConfig{
		AIProvider:  "ollama",
		AIModel:     "llama3",
		NixosFolder: "~/nixos-config",
		LogLevel:    "info",
		AIModels: AIModelsConfig{
			Providers: map[string]AIProviderConfig{
				"ollama": {
					Name:              "Ollama",
					Description:       "Local AI model provider for privacy-focused inference",
					Type:              "local",
					BaseURL:           "http://localhost:11434",
					Available:         true,
					SupportsStreaming: true,
					SupportsTools:     true,
					RequiresAPIKey:    false,
					Models: map[string]AIModelConfig{
						"llama3": {
							Name:             "Llama 3",
							Description:      "Meta's Llama 3 model for general-purpose tasks",
							Size:             "8B",
							Type:             "chat",
							ContextWindow:    8192,
							MaxTokens:        4096,
							RecommendedFor:   []string{"nixos", "general", "coding"},
							RequiresDownload: true,
							Default:          true,
						},
					},
				},
				"gemini": {
					Name:              "Google Gemini",
					Description:       "Google's advanced AI models via API",
					Type:              "cloud",
					BaseURL:           "https://generativelanguage.googleapis.com",
					Available:         true,
					SupportsStreaming: true,
					SupportsTools:     true,
					RequiresAPIKey:    true,
					EnvVar:            "GEMINI_API_KEY",
					Models: map[string]AIModelConfig{
						"gemini-1.5-flash": {
							Name:           "Gemini 1.5 Flash",
							Description:    "Fast and efficient Gemini model",
							Type:           "chat",
							ContextWindow:  1048576,
							MaxTokens:      8192,
							RecommendedFor: []string{"fast", "general", "nixos"},
							CostTier:       "standard",
							Default:        true,
						},
					},
				},
				"openai": {
					Name:              "OpenAI",
					Description:       "OpenAI's GPT models via API",
					Type:              "cloud",
					BaseURL:           "https://api.openai.com",
					Available:         true,
					SupportsStreaming: true,
					SupportsTools:     true,
					RequiresAPIKey:    true,
					EnvVar:            "OPENAI_API_KEY",
					Models: map[string]AIModelConfig{
						"gpt-3.5-turbo": {
							Name:           "GPT-3.5 Turbo",
							Description:    "Fast and cost-effective model",
							Type:           "chat",
							ContextWindow:  16385,
							MaxTokens:      4096,
							RecommendedFor: []string{"general", "fast", "budget"},
							CostTier:       "standard",
							Default:        true,
						},
					},
				},
				"bedrock": {
					Name:              "AWS Bedrock",
					Description:       "AWS Bedrock OpenAI-compatible endpoint",
					Type:              "cloud",
					BaseURL:           "https://bedrock-mantle.eu-north-1.api.aws/v1",
					Available:         true,
					SupportsStreaming: true,
					SupportsTools:     true,
					RequiresAPIKey:    true,
					EnvVar:            "AWS_BEDROCK_API_KEY",
					Models: map[string]AIModelConfig{
						"us.anthropic.claude-haiku-4-5-20251001-v1:0": {
							Name:           "Claude Haiku 4.5 (Bedrock)",
							Description:    "Anthropic Claude Haiku 4.5 via AWS Bedrock",
							Type:           "chat",
							ContextWindow:  200000,
							MaxTokens:      8192,
							RecommendedFor: []string{"fast", "general", "nixos"},
							CostTier:       "standard",
							Default:        true,
						},
						"openai.gpt-oss-120b-1:0": {
							Name:           "GPT OSS 120B (Bedrock)",
							Description:    "OpenAI GPT OSS 120B via AWS Bedrock",
							Type:           "chat",
							ContextWindow:  128000,
							MaxTokens:      8192,
							RecommendedFor: []string{"analysis", "complex", "coding"},
							CostTier:       "premium",
						},
					},
				},
				"copilot": {
					Name:              "GitHub Copilot",
					Description:       "GitHub Copilot's AI models with OpenAI compatibility",
					Type:              "cloud",
					BaseURL:           "https://api.githubcopilot.com",
					Available:         true,
					SupportsStreaming: true,
					SupportsTools:     true,
					RequiresAPIKey:    true,
					EnvVar:            "GITHUB_TOKEN",
					Models: map[string]AIModelConfig{
						"gpt-4": {
							Name:           "GPT-4 (Copilot)",
							Description:    "GPT-4 model via GitHub Copilot",
							Type:           "chat",
							ContextWindow:  128000,
							MaxTokens:      4096,
							RecommendedFor: []string{"coding", "nixos", "general", "analysis"},
							CostTier:       "premium",
							Default:        true,
						},
						"gpt-3.5-turbo": {
							Name:           "GPT-3.5 Turbo (Copilot)",
							Description:    "GPT-3.5 Turbo model via GitHub Copilot",
							Type:           "chat",
							ContextWindow:  16385,
							MaxTokens:      4096,
							RecommendedFor: []string{"coding", "fast", "general"},
							CostTier:       "standard",
						},
					},
				},
			},
			SelectionPreferences: AISelectionPreferences{
				DefaultProvider: "ollama",
				DefaultModels: map[string]string{
					"ollama":  "llama3",
					"gemini":  "gemini-1.5-flash",
					"openai":  "gpt-3.5-turbo",
					"bedrock": "us.anthropic.claude-haiku-4-5-20251001-v1:0",
					"copilot": "gpt-4",
				},
				TaskModels: map[string]TaskModelPreferences{
					"nixos_config": {
						Primary:  []string{"ollama:llama3", "gemini:gemini-1.5-flash"},
						Fallback: []string{"copilot:gpt-4", "openai:gpt-3.5-turbo"},
					},
					"general_help": {
						Primary:  []string{"ollama:llama3", "gemini:gemini-1.5-flash"},
						Fallback: []string{"copilot:gpt-3.5-turbo", "openai:gpt-3.5-turbo"},
					},
					"code_generation": {
						Primary:  []string{"copilot:gpt-4", "ollama:llama3"},
						Fallback: []string{"openai:gpt-4", "gemini:gemini-1.5-flash"},
					},
					"debugging": {
						Primary:  []string{"copilot:gpt-4", "ollama:llama3"},
						Fallback: []string{"openai:gpt-4", "gemini:gemini-1.5-flash"},
					},
				},
			},
			Discovery: AIDiscoveryConfig{
				AutoDiscover:  true,
				CacheDuration: 3600,
				CheckTimeout:  10,
				MaxRetries:    2,
			},
		},
		MCPServer: MCPServerConfig{
			Host:       "localhost",
			Port:       8081,
			MCPPort:    39847,
			SocketPath: "/tmp/nixai-mcp.sock",
			AutoStart:  false,
			DocumentationSources: []string{
				"https://wiki.nixos.org/wiki/NixOS_Wiki",
				"https://nix.dev/manual/nix",
				"https://nix.dev/",
				"https://nixos.org/manual/nixpkgs/stable/",
				"https://nix.dev/manual/nix/2.28/language/",
				"https://nix-community.github.io/home-manager/",
			},
		},
		Nixos: NixosConfig{
			ConfigPath: "~/nixos-config/configuration.nix",
			LogPath:    "/var/log/nixos/nixos-rebuild.log",
		},
		Diagnostics: DiagnosticsConfig{
			Enabled:   true,
			Threshold: 1,
			ErrorPatterns: []ErrorPatternConfig{
				{
					Name:        "example_error",
					Pattern:     "(?i)example error regex",
					ErrorType:   "custom",
					Severity:    "high",
					Description: "Example error description",
				},
			},
		},
		Commands: CommandsConfig{Timeout: 30, Retries: 2},
		AITimeouts: AITimeoutsConfig{
			Ollama:   60,
			LlamaCpp: 120,
			Gemini:   30,
			OpenAI:   30,
			Bedrock:  30,
			Custom:   60,
			Default:  60,
		},
		Devenv: DevenvConfig{
			DefaultDirectory: ".",
			AutoInitGit:      true,
			Templates: map[string]DevenvTemplateConfig{
				"python": {
					Enabled:               true,
					DefaultVersion:        "311",
					DefaultPackageManager: "pip",
				},
				"rust": {
					Enabled:        true,
					DefaultVersion: "stable",
				},
				"nodejs": {
					Enabled:               true,
					DefaultVersion:        "20",
					DefaultPackageManager: "npm",
				},
				"golang": {
					Enabled:        true,
					DefaultVersion: "1.21",
				},
			},
		},
		Discourse: DiscourseConfig{
			BaseURL:  "https://discourse.nixos.org",
			APIKey:   "", // Optional, can be set via environment variable
			Username: "", // Optional, can be set via environment variable
			Enabled:  true,
		},
		NixOSContext: NixOSContext{
			UsesFlakes:            false,
			UsesChannels:          false,
			NixOSConfigPath:       "",
			SystemType:            "unknown",
			HasHomeManager:        false,
			HomeManagerType:       "none",
			HomeManagerConfigPath: "",
			NixOSVersion:          "",
			NixVersion:            "",
			ConfigurationFiles:    []string{},
			EnabledServices:       []string{},
			InstalledPackages:     []string{},
			FlakeFile:             "",
			ConfigurationNix:      "",
			HardwareConfigNix:     "",
			LastDetected:          time.Time{},
			CacheValid:            false,
			DetectionErrors:       []string{},
		},
		Cache: CacheConfig{
			Enabled:         true,
			MemoryMaxSize:   1000, // 1000 entries in memory
			MemoryTTL:       30,   // 30 minutes for memory cache
			DiskEnabled:     true, // Enable disk persistence
			DiskPath:        "",   // Will be set to user cache dir
			DiskMaxSize:     100,  // 100MB disk cache limit
			DiskTTL:         24,   // 24 hours for disk cache
			CleanupInterval: 5,    // Cleanup every 5 minutes
			CompactInterval: 60,   // Compact every hour
		},
		Plugin: PluginConfig{
			Enabled:         true,
			Directory:       "~/nixai-plugins",
			CacheDirectory:  "~/nixai-plugins/cache",
			ConfigDirectory: "~/nixai-plugins/config",
			AutoDiscover:    true,
			AutoUpdate:      true,
			SandboxEnabled:  true,
			MaxConcurrent:   5,
			Timeout:         60,
			Repositories: []PluginRepositoryConfig{
				{
					Name:     "default",
					URL:      "https://plugins.nixai.org",
					Type:     "remote",
					Enabled:  true,
					Priority: 1,
					Verified: true,
				},
			},
			Marketplace: PluginMarketplaceConfig{
				Enabled:         true,
				BaseURL:         "https://marketplace.nixai.org",
				CacheDuration:   3600,
				FeaturedPlugins: 10,
				SearchTimeout:   30,
			},
			Security: PluginSecurityConfig{
				SandboxEnabled:       true,
				AllowNetwork:         false,
				AllowFilesystemWrite: false,
				AllowSystemCalls:     false,
				MaxMemoryMB:          512,
				MaxCPUPercent:        50,
				AllowedDomains:       []string{"nixai.org"},
				BlockedCapabilities:  []string{"exec", "fork"},
			},
			PackageManager: PluginPackageConfig{
				VerifySignatures:    true,
				AllowUnsigned:       false,
				UpdateCheckInterval: 1440,
				DownloadTimeout:     60,
				MaxDownloadSizeMB:   100,
			},
		},
	}
}

func ConfigFilePath() (string, error) {
	// Check for system-wide config first (for system services)
	systemConfig := "/etc/nixai/config.yaml"
	if _, err := os.Stat(systemConfig); err == nil {
		return systemConfig, nil
	}

	// Fall back to user config for normal user sessions
	usr, err := user.Current()
	if err != nil {
		// If we can't get user info (e.g., in systemd service), try system config
		return systemConfig, nil
	}
	configDir := filepath.Join(usr.HomeDir, ".config", "nixai")
	return filepath.Join(configDir, "config.yaml"), nil
}

// UserConfigFilePath always returns the user config path (for saving contexts and user data)
func UserConfigFilePath() (string, error) {
	usr, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("unable to determine user config path: %v", err)
	}
	configDir := filepath.Join(usr.HomeDir, ".config", "nixai")
	return filepath.Join(configDir, "config.yaml"), nil
}

func EnsureConfigFile() (string, error) {
	path, err := ConfigFilePath()
	if err != nil {
		return "", err
	}

	// Check if config file already exists
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}

	// If it's the system config path and doesn't exist, that's expected
	// The NixOS module will create it at system build time
	systemConfig := "/etc/nixai/config.yaml"
	if path == systemConfig {
		// For system services, don't try to create the config file
		// It should be created by the NixOS module
		return path, nil
	}

	// Try to create user config directory and file
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		// If we can't create the user config directory (e.g., in systemd service),
		// fall back to system config path if it exists
		if _, sysErr := os.Stat(systemConfig); sysErr == nil {
			return systemConfig, nil
		}
		return "", err
	}

	cfg := DefaultUserConfig()
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return "", err
	}

	// #nosec G306 -- Config files are not sensitive, 0644 is intentional for user config
	if err := os.WriteFile(path, data, 0600); err != nil {
		// If we can't write the user config file, fall back to system config if it exists
		if _, sysErr := os.Stat(systemConfig); sysErr == nil {
			return systemConfig, nil
		}
		return "", err
	}

	return path, nil
}

func LoadUserConfig() (*UserConfig, error) {
	path, err := EnsureConfigFile()
	if err != nil {
		return nil, err
	}
	// #nosec G304 -- Config file paths are validated and not user-supplied
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg UserConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func SaveUserConfig(cfg *UserConfig) error {
	path, err := UserConfigFilePath()
	if err != nil {
		return err
	}

	// Ensure the directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create config directory: %v", err)
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	// #nosec G306 -- Config files are not sensitive, 0644 is intentional for user config
	return os.WriteFile(path, data, 0600)
}

func LoadConfig(filePath string) (*Config, error) {
	// #nosec G304 -- Config file paths are validated and not user-supplied
	bytes, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var config Config
	if err := json.Unmarshal(bytes, &config); err != nil {
		return nil, err
	}

	return &config, nil
}

func SaveConfig(filePath string, config *Config) error {
	bytes, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	// #nosec G306 -- Config files are not sensitive, 0644 is intentional for user config
	return os.WriteFile(filePath, bytes, 0644)
}

func LoadYAMLConfig(filePath string) (*YAMLConfig, error) {
	// #nosec G304 -- Config file paths are validated and not user-supplied
	bytes, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var config struct {
		Default YAMLConfig `yaml:"default"`
	}
	if err := yaml.Unmarshal(bytes, &config); err != nil {
		return nil, err
	}

	return &config.Default, nil
}

// LoadEmbeddedYAMLConfig loads the embedded YAML configuration
func LoadEmbeddedYAMLConfig() (*YAMLConfig, error) {
	var config struct {
		Default YAMLConfig `yaml:"default"`
	}
	if err := yaml.Unmarshal([]byte(EmbeddedDefaultConfig), &config); err != nil {
		return nil, err
	}

	return &config.Default, nil
}

// EnsureConfigFileFromEmbedded creates user config from embedded default if it doesn't exist
func EnsureConfigFileFromEmbedded() (string, error) {
	path, err := ConfigFilePath()
	if err != nil {
		return "", err
	}

	// If config file already exists, return it
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}

	// If it's the system config path and doesn't exist, that's expected
	// The NixOS module will create it at system build time
	systemConfig := "/etc/nixai/config.yaml"
	if path == systemConfig {
		// For system services, don't try to create the config file
		// It should be created by the NixOS module
		return path, nil
	}

	// Try to create user config from embedded defaults
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		// If we can't create the user config directory (e.g., in systemd service),
		// fall back to system config path if it exists
		if _, sysErr := os.Stat(systemConfig); sysErr == nil {
			return systemConfig, nil
		}
		return "", err
	}

	// Parse embedded config and extract the content under 'default:' key
	embeddedCfg, err := LoadEmbeddedYAMLConfig()
	if err != nil {
		return "", err
	}

	// Convert to UserConfig structure and write as YAML
	userCfg := &UserConfig{
		AIProvider:  embeddedCfg.AIProvider,
		AIModel:     "llama3",         // Default model
		NixosFolder: "~/nixos-config", // Default folder
		LogLevel:    embeddedCfg.LogLevel,
		AIModels:    embeddedCfg.AIModels,
		MCPServer:   embeddedCfg.MCPServer,
		Nixos:       embeddedCfg.Nixos,
		Diagnostics: embeddedCfg.Diagnostics,
		Commands:    embeddedCfg.Commands,
		AITimeouts:  embeddedCfg.AITimeouts,
		Devenv:      embeddedCfg.Devenv,
		CustomAI:    embeddedCfg.CustomAI,
		Discourse:   embeddedCfg.Discourse,
		NixOSContext: NixOSContext{
			UsesFlakes:            false,
			UsesChannels:          false,
			NixOSConfigPath:       "",
			SystemType:            "unknown",
			HasHomeManager:        false,
			HomeManagerType:       "none",
			HomeManagerConfigPath: "",
			NixOSVersion:          "",
			NixVersion:            "",
			ConfigurationFiles:    []string{},
			EnabledServices:       []string{},
			InstalledPackages:     []string{},
			FlakeFile:             "",
			ConfigurationNix:      "",
			HardwareConfigNix:     "",
			LastDetected:          time.Time{},
			CacheValid:            false,
			DetectionErrors:       []string{},
		},
	}

	// Marshal to YAML and write to user config file
	data, err := yaml.Marshal(userCfg)
	if err != nil {
		return "", err
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		// If we can't write the user config file, fall back to system config if it exists
		if _, sysErr := os.Stat(systemConfig); sysErr == nil {
			return systemConfig, nil
		}
		return "", err
	}

	return path, nil
}
