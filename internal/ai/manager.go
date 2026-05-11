package ai

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"nix-ai-help/internal/cache"
	"nix-ai-help/internal/config"
	"nix-ai-help/internal/performance"
	"nix-ai-help/pkg/errors"
	"nix-ai-help/pkg/logger"
)

// ProviderPool manages connection pooling for AI providers
type ProviderPool struct {
	provider        Provider
	lastUsed        time.Time
	health          ProviderHealth
	lastHealthCheck time.Time
	initializing    bool
	mu              sync.RWMutex
}

// PoolConfig defines connection pooling settings
type PoolConfig struct {
	MaxIdleTime           time.Duration `json:"max_idle_time"`
	HealthCheckInterval   time.Duration `json:"health_check_interval"`
	InitializationTimeout time.Duration `json:"initialization_timeout"`
	LazyInitialization    bool          `json:"lazy_initialization"`
	MaxConcurrentChecks   int           `json:"max_concurrent_checks"`
}

// ProviderHealth represents health status of a provider
type ProviderHealth struct {
	Available   bool            `json:"available"`
	Latency     time.Duration   `json:"latency"`
	LastCheck   time.Time       `json:"last_check"`
	ErrorCount  int             `json:"error_count"`
	LastError   string          `json:"last_error,omitempty"`
	ModelStatus map[string]bool `json:"model_status,omitempty"`
	Initialized bool            `json:"initialized"`
}

// ProviderManager manages AI providers using the configuration system.
type ProviderManager struct {
	registry        *config.ModelRegistry
	config          *config.UserConfig
	providers       map[string]Provider      // Cache of initialized providers
	providerPools   map[string]*ProviderPool // Connection pools for providers
	cache           *cache.Manager           // Response cache manager
	monitor         *performance.Monitor     // Performance monitoring
	errorManager    *errors.ErrorManager     // Error handling and analytics
	logger          *logger.Logger
	executionConfig *ExecutionWrapperConfig // Execution wrapper configuration
	poolConfig      *PoolConfig             // Connection pooling configuration
	mu              sync.RWMutex            // Protects provider pools
}

// NewProviderManager creates a new provider manager with the given configuration.
func NewProviderManager(cfg *config.UserConfig, log *logger.Logger) *ProviderManager {
	if log == nil {
		log = logger.NewLogger()
	}

	registry := config.NewModelRegistry(cfg)

	// Initialize cache manager with configuration-based settings
	var cacheManager *cache.Manager
	if cfg.Cache.Enabled {
		// Convert config.CacheConfig to cache.ConfigCacheConfig
		configCache := cache.ConfigCacheConfig{
			Enabled:         cfg.Cache.Enabled,
			MemoryMaxSize:   cfg.Cache.MemoryMaxSize,
			MemoryTTL:       cfg.Cache.MemoryTTL,
			DiskEnabled:     cfg.Cache.DiskEnabled,
			DiskPath:        cfg.Cache.DiskPath,
			DiskMaxSize:     cfg.Cache.DiskMaxSize,
			DiskTTL:         cfg.Cache.DiskTTL,
			CleanupInterval: cfg.Cache.CleanupInterval,
			CompactInterval: cfg.Cache.CompactInterval,
		}

		cacheConfig := cache.FromConfigCacheConfig(configCache)
		var err error
		cacheManager, err = cache.NewManager(cacheConfig, log)
		if err != nil {
			log.Warn(fmt.Sprintf("Failed to initialize cache manager: %v", err))
			cacheManager = nil
		} else {
			log.Info("Cache manager initialized with user configuration")
		}
	} else {
		log.Info("Caching is disabled in configuration")
	}

	// Initialize error manager
	debugMode := cfg.LogLevel == "debug" || cfg.LogLevel == "trace"
	analyticsDir := filepath.Join(os.Getenv("HOME"), ".config", "nixai", "error_analytics")
	if home := os.Getenv("HOME"); home == "" {
		analyticsDir = "/tmp/nixai/error_analytics"
	}

	errorManagerConfig := &errors.ErrorManagerConfig{
		DebugMode:           debugMode,
		GracefulDegradation: true,
		AnalyticsEnabled:    true,
		AnalyticsDataDir:    analyticsDir,
		RetryConfig:         errors.DefaultRetryConfig(),
		MaxLastErrors:       50,
	}
	errorManager := errors.NewErrorManager(errorManagerConfig)

	// Initialize execution wrapper configuration
	executionConfig := &ExecutionWrapperConfig{
		Enabled:       cfg.Execution.Enabled,
		AutoExecute:   false, // Always start with safe defaults
		DryRunDefault: cfg.Execution.DryRunDefault,
		Patterns:      getDefaultExecutionPatternsStrings(),
	}

	// Default pool configuration
	poolConfig := &PoolConfig{
		MaxIdleTime:           5 * time.Minute,
		HealthCheckInterval:   30 * time.Second,
		InitializationTimeout: 10 * time.Second,
		LazyInitialization:    true,
		MaxConcurrentChecks:   3,
	}

	pm := &ProviderManager{
		registry:        registry,
		config:          cfg,
		providers:       make(map[string]Provider),
		providerPools:   make(map[string]*ProviderPool),
		cache:           cacheManager,
		monitor:         performance.NewMonitor(log),
		errorManager:    errorManager,
		logger:          log,
		executionConfig: executionConfig,
		poolConfig:      poolConfig,
	}

	// Initialize provider pools without creating providers (lazy loading)
	pm.setupProviderPools()

	// Start background health checker and cleanup
	go pm.backgroundHealthChecker()
	go pm.backgroundPoolCleanup()

	return pm
}

// setupProviderPools initializes provider pools without creating providers
func (pm *ProviderManager) setupProviderPools() {
	providers := pm.registry.GetAvailableProviders()

	pm.mu.Lock()
	defer pm.mu.Unlock()

	for _, providerName := range providers {
		if _, exists := pm.providerPools[providerName]; !exists {
			pm.providerPools[providerName] = &ProviderPool{
				health: ProviderHealth{
					Available:   false,
					Initialized: false,
					ModelStatus: make(map[string]bool),
				},
				lastHealthCheck: time.Time{}, // Force immediate health check when accessed
			}
			pm.logger.Debug(fmt.Sprintf("Set up provider pool for: %s", providerName))
		}
	}
}

// getOrCreateProviderPool gets or creates a provider pool
func (pm *ProviderManager) getOrCreateProviderPool(providerName string) (*ProviderPool, error) {
	pm.mu.RLock()
	pool, exists := pm.providerPools[providerName]
	pm.mu.RUnlock()

	if exists {
		return pool, nil
	}

	// Check if provider exists in configuration
	_, err := pm.registry.GetProvider(providerName)
	if err != nil {
		return nil, fmt.Errorf("provider '%s' is not configured: %w", providerName, err)
	}

	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Double-check after acquiring write lock
	if pool, exists := pm.providerPools[providerName]; exists {
		return pool, nil
	}

	// Create new pool
	pool = &ProviderPool{
		health: ProviderHealth{
			Available:   false,
			Initialized: false,
			ModelStatus: make(map[string]bool),
		},
		lastHealthCheck: time.Time{},
	}
	pm.providerPools[providerName] = pool
	pm.logger.Debug(fmt.Sprintf("Created new provider pool for: %s", providerName))

	return pool, nil
}

// initializeProviderWithTimeout initializes a provider with a timeout context
func (pm *ProviderManager) initializeProviderWithTimeout(ctx context.Context, providerName string) (Provider, error) {
	// Create a channel to receive the result
	resultChan := make(chan struct {
		provider Provider
		err      error
	}, 1)

	// Run initialization in a goroutine
	go func() {
		provider, err := pm.initializeProvider(providerName)

		// Wrap provider with execution awareness if enabled
		if err == nil && pm.executionConfig.Enabled {
			provider = NewExecutionAwareProvider(provider, pm.executionConfig, pm.logger)
			pm.logger.Debug(fmt.Sprintf("Wrapped provider %s with execution awareness", providerName))
		}

		resultChan <- struct {
			provider Provider
			err      error
		}{provider, err}
	}()

	// Wait for result or timeout
	select {
	case result := <-resultChan:
		return result.provider, result.err
	case <-ctx.Done():
		return nil, fmt.Errorf("provider initialization timed out: %w", ctx.Err())
	}
}

// performHealthCheck performs a health check on a provider pool
func (pm *ProviderManager) performHealthCheck(providerName string, pool *ProviderPool) {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	if pool.provider == nil {
		return // Skip health check for uninitialized providers
	}

	startTime := time.Now()
	pool.lastHealthCheck = startTime

	// Simple health check - try to get provider info
	_, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Check if provider is still responsive
	healthy := true
	var checkErr error

	// For providers that support health checking, we could add that here
	// For now, we'll just check if the provider is still accessible
	if pool.provider != nil {
		// Basic availability check - this could be enhanced per provider type
		pool.health.Available = true
		pool.health.Latency = time.Since(startTime)
		pool.health.LastCheck = time.Now()

		if checkErr != nil {
			pool.health.Available = false
			pool.health.LastError = checkErr.Error()
			pool.health.ErrorCount++
			healthy = false
		} else {
			pool.health.LastError = ""
		}
	}

	if healthy {
		pm.logger.Debug(fmt.Sprintf("Health check passed for provider: %s (latency: %v)",
			providerName, pool.health.Latency))
	} else {
		pm.logger.Warn(fmt.Sprintf("Health check failed for provider: %s - %s",
			providerName, pool.health.LastError))
	}
}

// backgroundHealthChecker runs periodic health checks
func (pm *ProviderManager) backgroundHealthChecker() {
	ticker := time.NewTicker(pm.poolConfig.HealthCheckInterval)
	defer ticker.Stop()

	for range ticker.C {
		pm.mu.RLock()
		pools := make(map[string]*ProviderPool)
		for name, pool := range pm.providerPools {
			pools[name] = pool
		}
		pm.mu.RUnlock()

		// Limit concurrent health checks
		semaphore := make(chan struct{}, pm.poolConfig.MaxConcurrentChecks)

		for providerName, pool := range pools {
			// Only check initialized providers
			pool.mu.RLock()
			needsCheck := pool.provider != nil &&
				time.Since(pool.lastHealthCheck) > pm.poolConfig.HealthCheckInterval
			pool.mu.RUnlock()

			if needsCheck {
				go func(name string, p *ProviderPool) {
					semaphore <- struct{}{}        // Acquire
					defer func() { <-semaphore }() // Release
					pm.performHealthCheck(name, p)
				}(providerName, pool)
			}
		}
	}
}

// backgroundPoolCleanup removes idle providers from memory
func (pm *ProviderManager) backgroundPoolCleanup() {
	ticker := time.NewTicker(2 * pm.poolConfig.MaxIdleTime)
	defer ticker.Stop()

	for range ticker.C {
		pm.cleanupIdlePools()
	}
}

// cleanupIdlePools removes providers that have been idle for too long
func (pm *ProviderManager) cleanupIdlePools() {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for providerName, pool := range pm.providerPools {
		pool.mu.Lock()

		// Clean up idle providers to free memory
		if pool.provider != nil &&
			time.Since(pool.lastUsed) > pm.poolConfig.MaxIdleTime {

			pm.logger.Debug(fmt.Sprintf("Cleaning up idle provider: %s", providerName))

			// Reset the pool but keep the structure for lazy reinitialization
			pool.provider = nil
			pool.health.Initialized = false
			pool.health.Available = false
		}

		pool.mu.Unlock()
	}
}

// GetProviderHealth returns the health status of a provider
func (pm *ProviderManager) GetProviderHealth(providerName string) (*ProviderHealth, error) {
	pm.mu.RLock()
	pool, exists := pm.providerPools[providerName]
	pm.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("provider %s not found", providerName)
	}

	pool.mu.RLock()
	defer pool.mu.RUnlock()

	// Create a copy to avoid race conditions
	health := pool.health
	return &health, nil
}

// GetAllProviderHealth returns health status for all providers
func (pm *ProviderManager) GetAllProviderHealth() map[string]*ProviderHealth {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	result := make(map[string]*ProviderHealth)
	for name, pool := range pm.providerPools {
		pool.mu.RLock()
		health := pool.health
		pool.mu.RUnlock()
		result[name] = &health
	}

	return result
}

// GetProvider retrieves or initializes a provider by name with connection pooling.
func (pm *ProviderManager) GetProvider(providerName string) (Provider, error) {
	// Get from connection pool with lazy initialization
	pool, err := pm.getOrCreateProviderPool(providerName)
	if err != nil {
		return nil, err
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()

	// Initialize provider if not already done (lazy initialization)
	if pool.provider == nil && !pool.initializing {
		pool.initializing = true
		pm.logger.Debug(fmt.Sprintf("Lazy initializing provider: %s", providerName))

		// Initialize with timeout
		ctx, cancel := context.WithTimeout(context.Background(), pm.poolConfig.InitializationTimeout)
		defer cancel()

		provider, initErr := pm.initializeProviderWithTimeout(ctx, providerName)
		pool.initializing = false

		if initErr != nil {
			pool.health.Available = false
			pool.health.LastError = initErr.Error()
			pool.health.ErrorCount++
			return nil, fmt.Errorf("failed to initialize provider '%s': %w", providerName, initErr)
		}

		pool.provider = provider
		pool.health.Initialized = true
		pool.health.Available = true
		pool.lastUsed = time.Now()
		pm.logger.Info(fmt.Sprintf("Successfully initialized provider: %s", providerName))
	}

	// Wait if another goroutine is initializing
	if pool.initializing {
		pool.mu.Unlock()
		time.Sleep(100 * time.Millisecond) // Brief wait
		pool.mu.Lock()
		if pool.provider == nil {
			return nil, fmt.Errorf("provider %s initialization failed or timed out", providerName)
		}
	}

	// Update last used time
	pool.lastUsed = time.Now()

	// Schedule health check if needed
	if time.Since(pool.lastHealthCheck) > pm.poolConfig.HealthCheckInterval {
		go pm.performHealthCheck(providerName, pool)
	}

	return pool.provider, nil
}

// GetProviderWithModel retrieves a provider configured for a specific model.
func (pm *ProviderManager) GetProviderWithModel(providerName, modelName string) (Provider, error) {
	// Validate that the model exists for this provider
	model, err := pm.registry.GetModel(providerName, modelName)
	if err != nil {
		return nil, fmt.Errorf("model '%s' not found for provider '%s': %w", modelName, providerName, err)
	}

	// Get the provider
	provider, err := pm.GetProvider(providerName)
	if err != nil {
		return nil, err
	}

	// For providers that support model selection, we'll need to wrap them
	// This will be implemented based on the specific provider interface
	pm.logger.Debug(fmt.Sprintf("Using model '%s' with provider '%s'", model.Name, providerName))

	return provider, nil
}

// GetDefaultProvider retrieves the default provider as configured.
func (pm *ProviderManager) GetDefaultProvider() (Provider, error) {
	defaultProvider := pm.config.AIModels.SelectionPreferences.DefaultProvider
	if defaultProvider == "" {
		defaultProvider = "ollama" // Final fallback
	}

	return pm.GetProvider(defaultProvider)
}

// GetRecommendedProvider retrieves the recommended provider for a specific task.
func (pm *ProviderManager) GetRecommendedProvider(task string) (Provider, string, error) {
	// Get recommended model for the task
	providerName, modelName, err := pm.registry.GetRecommendedModelForTask(task)
	if err != nil {
		// Fall back to default provider
		provider, err := pm.GetDefaultProvider()
		if err != nil {
			return nil, "", err
		}
		return provider, "", err
	}

	// Get provider with specific model
	provider, err := pm.GetProviderWithModel(providerName, modelName)
	if err != nil {
		return nil, "", err
	}

	return provider, modelName, nil
}

// GetAvailableProviders returns a list of all available providers.
func (pm *ProviderManager) GetAvailableProviders() []string {
	return pm.registry.GetAvailableProviders()
}

// GetAvailableModels returns a list of all available models for a provider.
func (pm *ProviderManager) GetAvailableModels(providerName string) ([]string, error) {
	return pm.registry.GetAvailableModels(providerName)
}

// GetProviderInfo returns information about a specific provider.
func (pm *ProviderManager) GetProviderInfo(providerName string) (*config.AIProviderConfig, error) {
	return pm.registry.GetProvider(providerName)
}

// GetModelInfo returns information about a specific model.
func (pm *ProviderManager) GetModelInfo(providerName, modelName string) (*config.AIModelConfig, error) {
	return pm.registry.GetModel(providerName, modelName)
}

// CheckProviderStatus checks the status of a provider (e.g., if it's running).
func (pm *ProviderManager) CheckProviderStatus(providerName string) (bool, error) {
	available := pm.registry.IsProviderAvailable(providerName)
	return available, nil
}

// ValidateConfiguration validates the current AI models configuration.
func (pm *ProviderManager) ValidateConfiguration() error {
	return pm.registry.ValidateConfiguration()
}

// RefreshProviders clears the provider cache, forcing reinitialization.
func (pm *ProviderManager) RefreshProviders() {
	pm.providers = make(map[string]Provider)
	pm.logger.Info("Provider cache cleared")
}

// initializeProvider creates a new provider instance based on configuration.
func (pm *ProviderManager) initializeProvider(providerName string) (Provider, error) {
	providerConfig, err := pm.registry.GetProvider(providerName)
	if err != nil {
		return nil, err
	}

	switch providerName {
	case "ollama":
		return pm.initializeOllamaProvider(providerConfig)
	case "gemini":
		return pm.initializeGeminiProvider(providerConfig)
	case "openai":
		return pm.initializeOpenAIProvider(providerConfig)
	case "copilot":
		return pm.initializeCopilotProvider(providerConfig)
	case "claude":
		return pm.initializeClaudeProvider(providerConfig)
	case "groq":
		return pm.initializeGroqProvider(providerConfig)
	case "bedrock":
		return pm.initializeBedrockProvider(providerConfig)
	case "llamacpp":
		return pm.initializeLlamaCppProvider(providerConfig)
	case "custom":
		return pm.initializeCustomProvider(providerConfig)
	default:
		return nil, fmt.Errorf("unknown provider type: %s", providerName)
	}
}

// initializeOllamaProvider creates an Ollama provider instance.
func (pm *ProviderManager) initializeOllamaProvider(config *config.AIProviderConfig) (Provider, error) {
	// Get default model for Ollama
	defaultModel := pm.config.AIModels.SelectionPreferences.DefaultModels["ollama"]
	if defaultModel == "" {
		defaultModel = "llama3" // fallback
	}

	// Set custom endpoint if configured
	if config.BaseURL != "" {
		os.Setenv("OLLAMA_ENDPOINT", config.BaseURL+"/api/generate")
	}

	ollamaProvider := NewOllamaProvider(defaultModel)

	// Apply configured timeout
	timeout := pm.config.GetAITimeout("ollama")
	ollamaProvider.SetTimeout(timeout)

	pm.logger.Debug(fmt.Sprintf("Ollama provider initialized with model '%s' and %v timeout", defaultModel, timeout))

	// Check if Ollama is accessible and validate the model
	if err := ollamaProvider.HealthCheck(); err != nil {
		pm.logger.Debug(fmt.Sprintf("Ollama health check failed: %v", err))
		// Don't fail initialization, just log the issue
	} else {
		// If health check passes, try to validate the model
		if err := ollamaProvider.ValidateModel(); err != nil {
			pm.logger.Warn(fmt.Sprintf("Ollama model validation failed: %v", err))

			// Try to auto-detect and use the first available model
			if availableModels, getErr := ollamaProvider.GetAvailableModels(); getErr == nil && len(availableModels) > 0 {
				firstAvailable := availableModels[0]
				pm.logger.Info(fmt.Sprintf("Auto-switching to available model: %s", firstAvailable))
				ollamaProvider.SetModel(firstAvailable)
			} else {
				pm.logger.Warn("Could not auto-detect available models. Provider may fail at runtime.")
			}
		} else {
			pm.logger.Debug(fmt.Sprintf("Ollama model '%s' validated successfully", defaultModel))
		}
	}

	// Create legacy wrapper and then wrap that as Provider
	legacyProvider := &OllamaLegacyProvider{OllamaProvider: ollamaProvider}
	return NewProviderWrapper(legacyProvider), nil
}

// initializeGeminiProvider creates a Gemini provider instance.
func (pm *ProviderManager) initializeGeminiProvider(config *config.AIProviderConfig) (Provider, error) {
	apiKey := os.Getenv(config.EnvVar)
	if apiKey == "" && config.RequiresAPIKey {
		return nil, fmt.Errorf("gemini API key not found in environment variable %s", config.EnvVar)
	}

	baseURL := config.BaseURL
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash-preview-05-20:generateContent"
	}

	// Get default model for Gemini
	defaultModel := pm.config.AIModels.SelectionPreferences.DefaultModels["gemini"]
	if defaultModel == "" {
		defaultModel = "gemini-pro" // fallback
	}

	geminiClient := NewGeminiClientWithModel(apiKey, baseURL, defaultModel)
	return NewProviderWrapper(geminiClient), nil
}

// initializeOpenAIProvider creates an OpenAI provider instance.
func (pm *ProviderManager) initializeOpenAIProvider(config *config.AIProviderConfig) (Provider, error) {
	apiKey := os.Getenv(config.EnvVar)
	if apiKey == "" && config.RequiresAPIKey {
		return nil, fmt.Errorf("openAI API key not found in environment variable %s", config.EnvVar)
	}

	// Get default model for OpenAI
	defaultModel := pm.config.AIModels.SelectionPreferences.DefaultModels["openai"]
	if defaultModel == "" {
		defaultModel = "gpt-3.5-turbo" // fallback
	}

	openaiClient := NewOpenAIClientWithModel(apiKey, defaultModel)
	return NewProviderWrapper(openaiClient), nil
}

// initializeCopilotProvider creates a GitHub Copilot provider instance.
func (pm *ProviderManager) initializeCopilotProvider(config *config.AIProviderConfig) (Provider, error) {
	apiKey := os.Getenv(config.EnvVar)
	// Note: We allow initialization even without API key - the error will be reported during actual API calls

	// Get default model for Copilot
	defaultModel := pm.config.AIModels.SelectionPreferences.DefaultModels["copilot"]
	if defaultModel == "" {
		defaultModel = "gpt-4" // fallback for GitHub Copilot
	}

	copilotClient := NewCopilotClientWithModel(apiKey, defaultModel)
	return NewProviderWrapper(copilotClient), nil
}

// initializeLlamaCppProvider creates a LlamaCpp provider instance.
func (pm *ProviderManager) initializeLlamaCppProvider(config *config.AIProviderConfig) (Provider, error) {
	// Get default model for LlamaCpp
	defaultModel := pm.config.AIModels.SelectionPreferences.DefaultModels["llamacpp"]
	if defaultModel == "" {
		defaultModel = "llama3" // fallback
	}

	// Use the new model-aware constructor
	llamacppProvider, err := NewLlamaCppProviderWithModel(config, defaultModel)
	if err != nil {
		// Fall back to simple constructor if model-aware fails
		llamacppProvider = NewLlamaCppProvider(defaultModel)
	}

	// Apply configured timeout
	timeout := pm.config.GetAITimeout("llamacpp")
	llamacppProvider.SetTimeout(timeout)

	pm.logger.Debug(fmt.Sprintf("LlamaCpp provider initialized with %v timeout", timeout))

	return NewProviderWrapper(llamacppProvider), nil
}

// initializeCustomProvider creates a custom provider instance.
func (pm *ProviderManager) initializeCustomProvider(config *config.AIProviderConfig) (Provider, error) {
	if config.BaseURL == "" {
		return nil, fmt.Errorf("custom provider requires a base URL")
	}

	// Get default model for Custom provider
	defaultModel := pm.config.AIModels.SelectionPreferences.DefaultModels["custom"]
	if defaultModel == "" {
		// Find first available model in configuration
		for modelName := range config.Models {
			defaultModel = modelName
			break
		}
		if defaultModel == "" {
			defaultModel = "default" // fallback
		}
	}

	// Use the new model-aware constructor
	customProvider, err := NewCustomProviderWithModel(config, defaultModel)
	if err != nil {
		// Fall back to simple constructor if model-aware fails
		var headers map[string]string
		if pm.config.CustomAI.Headers != nil {
			headers = pm.config.CustomAI.Headers
		} else {
			headers = make(map[string]string)
		}
		customProvider = NewCustomProvider(config.BaseURL, headers)
	}

	// Apply configured timeout
	timeout := pm.config.GetAITimeout("custom")
	customProvider.SetTimeout(timeout)

	pm.logger.Debug(fmt.Sprintf("Custom provider initialized with %v timeout", timeout))

	return NewProviderWrapper(customProvider), nil
}

// initializeClaudeProvider creates a Claude provider instance.
func (pm *ProviderManager) initializeClaudeProvider(config *config.AIProviderConfig) (Provider, error) {
	apiKey := os.Getenv(config.EnvVar)
	if apiKey == "" && config.RequiresAPIKey {
		return nil, fmt.Errorf("Claude API key not found in environment variable %s", config.EnvVar)
	}

	// Get default model for Claude
	defaultModel := pm.config.AIModels.SelectionPreferences.DefaultModels["claude"]
	if defaultModel == "" {
		defaultModel = "claude-3-sonnet-20240229" // fallback
	}

	// Use the new model-aware constructor
	claudeClient, err := NewClaudeProviderWithModel(config, defaultModel)
	if err != nil {
		// Fall back to simple constructor if model-aware fails
		claudeClient = NewClaudeClientWithModel(apiKey, defaultModel)
	}

	// Apply configured timeout
	timeout := pm.config.GetAITimeout("claude")
	claudeClient.SetTimeout(timeout)

	pm.logger.Debug(fmt.Sprintf("Claude provider initialized with %v timeout", timeout))

	// Create legacy wrapper and then wrap that as Provider
	legacyProvider := &ClaudeLegacyProvider{ClaudeClient: claudeClient}
	return NewProviderWrapper(legacyProvider), nil
}

// initializeBedrockProvider creates an AWS Bedrock provider instance.

func (pm *ProviderManager) initializeBedrockProvider(providerConfig *config.AIProviderConfig) (Provider, error) {
	apiKey := os.Getenv(providerConfig.EnvVar)
	if apiKey == "" && providerConfig.RequiresAPIKey {
		return nil, fmt.Errorf("bedrock API key not found in environment variable %s", providerConfig.EnvVar)
	}

	baseURL := providerConfig.BaseURL
	if baseURL == "" {
		baseURL = "https://bedrock-mantle.eu-north-1.api.aws/v1"
	}

	defaultModel := pm.config.AIModels.SelectionPreferences.DefaultModels["bedrock"]
	if defaultModel == "" {
		for modelName := range providerConfig.Models {
			defaultModel = modelName
			break
		}
	}
	if defaultModel == "" {
		return nil, fmt.Errorf("no default model configured for bedrock")
	}

	modelConfig, exists := providerConfig.Models[defaultModel]
	resolvedModelName := defaultModel
	if !exists {
		if alias, ok := resolveBedrockModelAlias(providerConfig.Models, defaultModel); ok {
			modelConfig = providerConfig.Models[alias]
			exists = true
			resolvedModelName = alias
		}
	}
	if !exists {
		return nil, fmt.Errorf("bedrock model '%s' not found in configuration", defaultModel)
	}

	costConfig := BedrockCostConfig{
		PromptPer1M:       modelConfig.PromptCostPer1M,
		CompletionPer1M:   modelConfig.CompletionCostPer1M,
		CachedPromptPer1M: modelConfig.CachedPromptCostPer1M,
	}
	if costConfig.PromptPer1M == 0 || costConfig.CompletionPer1M == 0 || costConfig.CachedPromptPer1M == 0 {
		defaults, err := config.LoadEmbeddedYAMLConfig()
		if err == nil {
			if defaultProvider, ok := defaults.AIModels.Providers["bedrock"]; ok {
				modelName := resolvedModelName
				if _, ok := defaultProvider.Models[modelName]; !ok {
					if alias, ok := resolveBedrockModelAlias(defaultProvider.Models, modelName); ok {
						modelName = alias
					}
				}
				if defaultModelConfig, ok := defaultProvider.Models[modelName]; ok {
					if costConfig.PromptPer1M == 0 {
						costConfig.PromptPer1M = defaultModelConfig.PromptCostPer1M
					}
					if costConfig.CompletionPer1M == 0 {
						costConfig.CompletionPer1M = defaultModelConfig.CompletionCostPer1M
					}
					if costConfig.CachedPromptPer1M == 0 {
						costConfig.CachedPromptPer1M = defaultModelConfig.CachedPromptCostPer1M
					}
				}
			}
		}
	}
	if costConfig.CachedPromptPer1M == 0 {
		costConfig.CachedPromptPer1M = costConfig.PromptPer1M
	}

	bedrockClient := NewBedrockClient(apiKey, baseURL, defaultModel, costConfig, pm.logger)
	return NewProviderWrapper(bedrockClient), nil
}

func resolveBedrockModelAlias(models map[string]config.AIModelConfig, modelName string) (string, bool) {
	if _, ok := models[modelName]; ok {
		return modelName, true
	}

	candidates := []string{
		strings.TrimSuffix(modelName, ":0"),
		strings.TrimSuffix(modelName, "-1:0"),
		strings.TrimSuffix(modelName, "-1"),
		strings.TrimPrefix(modelName, "us."),
	}

	if !strings.Contains(modelName, ":") {
		candidates = append(candidates, modelName+":0", modelName+"-1:0", "us."+modelName)
	}

	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, ok := models[candidate]; ok {
			return candidate, true
		}
	}

	return "", false
}

// initializeGroqProvider creates a Groq provider instance.
func (pm *ProviderManager) initializeGroqProvider(config *config.AIProviderConfig) (Provider, error) {
	apiKey := os.Getenv(config.EnvVar)
	if apiKey == "" && config.RequiresAPIKey {
		return nil, fmt.Errorf("Groq API key not found in environment variable %s", config.EnvVar)
	}

	// Get default model for Groq
	defaultModel := pm.config.AIModels.SelectionPreferences.DefaultModels["groq"]
	if defaultModel == "" {
		defaultModel = "llama3-8b-8192" // fallback
	}

	// Use the new model-aware constructor
	groqClient, err := NewGroqProviderWithModel(config, defaultModel)
	if err != nil {
		// Fall back to simple constructor if model-aware fails
		groqClient = NewGroqClientWithModel(apiKey, defaultModel)
	}

	// Apply configured timeout
	timeout := pm.config.GetAITimeout("groq")
	groqClient.SetTimeout(timeout)

	pm.logger.Debug(fmt.Sprintf("Groq provider initialized with %v timeout", timeout))

	// Create legacy wrapper and then wrap that as Provider
	legacyProvider := &GroqLegacyProvider{GroqClient: groqClient}
	return NewProviderWrapper(legacyProvider), nil
}

// parseModelReference parses a model reference in the format "provider:model".
func parseModelReference(modelRef string) (provider, model string, err error) {
	// Handle empty reference
	if modelRef == "" {
		return "ollama", "llama3", nil
	}

	// Check if it contains provider:model format
	parts := strings.Split(modelRef, ":")
	if len(parts) == 2 {
		provider = strings.TrimSpace(parts[0])
		model = strings.TrimSpace(parts[1])

		// Validate provider and model are not empty
		if provider == "" || model == "" {
			return "", "", fmt.Errorf("invalid model reference format: '%s'", modelRef)
		}

		return provider, model, nil
	} else if len(parts) == 1 {
		// Just a model name, use default provider
		model = strings.TrimSpace(parts[0])
		if model == "" {
			return "", "", fmt.Errorf("empty model name in reference: '%s'", modelRef)
		}
		return "ollama", model, nil
	}

	return "", "", fmt.Errorf("invalid model reference format: '%s' (expected 'provider:model' or 'model')", modelRef)
}

// Legacy compatibility methods

// CreateLegacyProvider creates a legacy AIProvider for backward compatibility.
func (pm *ProviderManager) CreateLegacyProvider(providerName string) (AIProvider, error) {
	provider, err := pm.GetProvider(providerName)
	if err != nil {
		return nil, err
	}

	// If it's a wrapper, extract the legacy provider
	if wrapper, ok := provider.(*ProviderWrapper); ok {
		return wrapper.legacy, nil
	}

	// If it's already adapted, extract the legacy provider
	if adapter, ok := provider.(*LegacyProviderAdapter); ok {
		return adapter.legacy, nil
	}

	// Otherwise, create a reverse adapter (Provider -> AIProvider)
	return &ProviderToLegacyAdapter{provider: provider}, nil
}

// ProviderToLegacyAdapter adapts a new Provider to the legacy AIProvider interface.
type ProviderToLegacyAdapter struct {
	provider Provider
}

// Query implements the legacy AIProvider interface.
func (a *ProviderToLegacyAdapter) Query(prompt string) (string, error) {
	// Try context-aware QueryWithContext first
	if p, ok := a.provider.(interface {
		QueryWithContext(context.Context, string) (string, error)
	}); ok {
		return p.QueryWithContext(context.Background(), prompt)
	}
	// Fallback to legacy Query(prompt string)
	if p, ok := a.provider.(interface{ Query(string) (string, error) }); ok {
		return p.Query(prompt)
	}
	return "", fmt.Errorf("underlying provider does not implement QueryWithContext or Query")
}

// HealthChecker interface for providers that support health checking
type HealthChecker interface {
	HealthCheck() error
}

// GetHealthyProvider retrieves a provider and ensures it's healthy, with fallback
func (pm *ProviderManager) GetHealthyProvider(providerName string) (Provider, error) {
	provider, err := pm.GetProvider(providerName)
	if err != nil {
		return nil, err
	}

	// Check health if provider supports it
	if healthChecker, ok := provider.(HealthChecker); ok {
		if err := healthChecker.HealthCheck(); err != nil {
			pm.logger.Warn(fmt.Sprintf("Provider %s failed health check: %v", providerName, err))

			// Try fallback providers
			fallbackProviders := pm.getFallbackProviders(providerName)
			for _, fallback := range fallbackProviders {
				pm.logger.Info(fmt.Sprintf("Trying fallback provider: %s", fallback))
				if fallbackProvider, err := pm.GetProvider(fallback); err == nil {
					if fallbackChecker, ok := fallbackProvider.(HealthChecker); ok {
						if err := fallbackChecker.HealthCheck(); err == nil {
							pm.logger.Info(fmt.Sprintf("Successfully fell back to provider: %s", fallback))
							return fallbackProvider, nil
						}
					} else {
						// Assume healthy if no health check method
						pm.logger.Info(fmt.Sprintf("Successfully fell back to provider: %s", fallback))
						return fallbackProvider, nil
					}
				}
			}

			return nil, fmt.Errorf("provider %s is unhealthy and no fallback available: %w", providerName, err)
		}
	}

	return provider, nil
}

// getFallbackProviders returns a list of fallback providers for the given provider
func (pm *ProviderManager) getFallbackProviders(providerName string) []string {
	// Get fallback providers from configuration or use defaults
	var fallbacks []string

	// Check if there are task-specific fallbacks configured
	for _, taskPrefs := range pm.config.AIModels.SelectionPreferences.TaskModels {
		for _, fallback := range taskPrefs.Fallback {
			if provider, _, err := parseModelReference(fallback); err == nil && provider != providerName {
				fallbacks = append(fallbacks, provider)
			}
		}
	}

	// Add default fallbacks if none configured
	if len(fallbacks) == 0 {
		switch providerName {
		case "gemini", "openai":
			fallbacks = []string{"ollama"}
		case "ollama":
			fallbacks = []string{"gemini", "openai"}
		case "llamacpp":
			fallbacks = []string{"ollama"}
		case "custom":
			fallbacks = []string{"ollama"}
		}
	}

	// Remove duplicates and the original provider
	seen := make(map[string]bool)
	var uniqueFallbacks []string
	for _, fb := range fallbacks {
		if !seen[fb] && fb != providerName {
			seen[fb] = true
			uniqueFallbacks = append(uniqueFallbacks, fb)
		}
	}

	return uniqueFallbacks
}

// GetProviderForTask retrieves the best provider for a specific task with fallback
func (pm *ProviderManager) GetProviderForTask(task string) (Provider, string, error) {
	// First try to get recommended provider for task
	if _, model, err := pm.GetRecommendedProvider(task); err == nil {
		// Try to get healthy version
		providerName := getProviderFromModel(model)
		if healthyProvider, err := pm.GetHealthyProvider(providerName); err == nil {
			return healthyProvider, model, nil
		}
		pm.logger.Warn(fmt.Sprintf("Recommended provider for task %s is unhealthy, trying alternatives", task))
	}

	// Fall back to default provider
	defaultProvider, err := pm.GetDefaultProvider()
	if err != nil {
		return nil, "", fmt.Errorf("failed to get default provider: %w", err)
	}

	return defaultProvider, "", nil
}

// getProviderFromModel extracts provider name from model reference
func getProviderFromModel(model string) string {
	if provider, _, err := parseModelReference(model); err == nil {
		return provider
	}
	return "ollama" // default fallback
}

// QueryWithCache attempts to get a cached response first, then queries the provider
func (pm *ProviderManager) QueryWithCache(ctx context.Context, providerName, modelName, prompt string) (string, error) {
	// Start performance monitoring
	operationName := fmt.Sprintf("ai_query_%s_%s", providerName, modelName)
	finishTimer := pm.monitor.StartTimer(performance.MetricAIQuery, operationName, map[string]string{
		"provider": providerName,
		"model":    modelName,
	})

	// Try to get cached response first
	if pm.cache != nil {
		if cachedResponse, found := pm.cache.GetAIResponse(ctx, providerName, modelName, prompt); found {
			pm.logger.Debug(fmt.Sprintf("Cache hit for AI query (provider: %s, model: %s)", providerName, modelName))

			// Record cache hit
			pm.monitor.RecordMetric(performance.Metric{
				Type: performance.MetricCacheHit,
				Name: operationName,
				Tags: map[string]string{
					"provider": providerName,
					"model":    modelName,
				},
				Success: true,
			})

			finishTimer(true, nil)
			return string(cachedResponse), nil
		}

		// Record cache miss
		pm.monitor.RecordMetric(performance.Metric{
			Type: performance.MetricCacheMiss,
			Name: operationName,
			Tags: map[string]string{
				"provider": providerName,
				"model":    modelName,
			},
			Success: true,
		})
	}

	// Cache miss or no cache available, query the provider
	provider, providerErr := pm.GetProviderWithModel(providerName, modelName)
	if providerErr != nil {
		finishTimer(false, providerErr)
		return "", providerErr
	}

	// Try context-aware methods first, then fallback to legacy
	var response string
	var err error

	if p, ok := provider.(interface {
		GenerateResponse(context.Context, string) (string, error)
	}); ok {
		response, err = p.GenerateResponse(ctx, prompt)
	} else if p, ok := provider.(interface {
		QueryWithContext(context.Context, string) (string, error)
	}); ok {
		response, err = p.QueryWithContext(ctx, prompt)
	} else {
		// Fallback to legacy Query method
		response, err = provider.Query(prompt)
	}

	if err != nil {
		finishTimer(false, err)
		return "", err
	}

	// Cache the successful response
	if pm.cache != nil {
		if err := pm.cache.SetAIResponse(ctx, providerName, modelName, prompt, []byte(response)); err != nil {
			pm.logger.Debug(fmt.Sprintf("Failed to cache AI response: %v", err))
		} else {
			pm.logger.Debug(fmt.Sprintf("Cached AI response (provider: %s, model: %s)", providerName, modelName))
		}
	}

	finishTimer(true, nil)
	return response, nil
}

// GetCacheStats returns cache statistics
func (pm *ProviderManager) GetCacheStats() *cache.CombinedCacheStats {
	if pm.cache == nil {
		return nil
	}

	stats := pm.cache.Stats()
	return &stats
}

// ClearCache clears all cached AI responses
func (pm *ProviderManager) ClearCache(ctx context.Context) error {
	if pm.cache == nil {
		return fmt.Errorf("cache not available")
	}

	return pm.cache.Clear(ctx)
}

// Close gracefully shuts down the provider manager and cache
func (pm *ProviderManager) Close() error {
	if pm.cache != nil {
		return pm.cache.Close()
	}
	return nil
}

// GetPerformanceStats returns comprehensive performance statistics
func (pm *ProviderManager) GetPerformanceStats() performance.MetricsSummary {
	return pm.monitor.GetSummary()
}

// GetCachePerformance returns cache-specific performance metrics
func (pm *ProviderManager) GetCachePerformance() performance.CachePerformance {
	cacheStats := pm.GetCacheStats()
	return pm.monitor.GetCachePerformance(cacheStats)
}

// FormatPerformanceReport returns a human-readable performance report
func (pm *ProviderManager) FormatPerformanceReport() string {
	return pm.monitor.FormatSummary()
}

// ResetPerformanceMetrics clears all performance metrics (useful for testing)
func (pm *ProviderManager) ResetPerformanceMetrics() {
	pm.monitor.Reset()
}

// ParallelQueryResult represents the result of a parallel query
type ParallelQueryResult struct {
	ProviderName string
	ModelName    string
	Prompt       string
	Response     string
	Error        error
	Duration     time.Duration
}

// ParallelQuery executes multiple AI queries concurrently
func (pm *ProviderManager) ParallelQuery(ctx context.Context, queries []struct {
	ProviderName string
	ModelName    string
	Prompt       string
}) []ParallelQueryResult {
	results := make([]ParallelQueryResult, len(queries))
	var wg sync.WaitGroup

	// Limit concurrent operations to avoid overwhelming providers
	semaphore := make(chan struct{}, 5) // Max 5 concurrent queries

	for i, query := range queries {
		wg.Add(1)
		go func(index int, q struct {
			ProviderName string
			ModelName    string
			Prompt       string
		}) {
			defer wg.Done()
			semaphore <- struct{}{}        // Acquire semaphore
			defer func() { <-semaphore }() // Release semaphore

			start := time.Now()
			response, err := pm.QueryWithCache(ctx, q.ProviderName, q.ModelName, q.Prompt)
			duration := time.Since(start)

			results[index] = ParallelQueryResult{
				ProviderName: q.ProviderName,
				ModelName:    q.ModelName,
				Prompt:       q.Prompt,
				Response:     response,
				Error:        err,
				Duration:     duration,
			}
		}(i, query)
	}

	wg.Wait()
	return results
}

// BatchQuerySameSources executes the same query across multiple providers/models
func (pm *ProviderManager) BatchQuerySameSources(ctx context.Context, prompt string, sources []struct {
	ProviderName string
	ModelName    string
}) []ParallelQueryResult {
	queries := make([]struct {
		ProviderName string
		ModelName    string
		Prompt       string
	}, len(sources))

	for i, source := range sources {
		queries[i] = struct {
			ProviderName string
			ModelName    string
			Prompt       string
		}{
			ProviderName: source.ProviderName,
			ModelName:    source.ModelName,
			Prompt:       prompt,
		}
	}

	return pm.ParallelQuery(ctx, queries)
}

// QueryWithFallback attempts multiple providers in parallel and returns the first successful result
func (pm *ProviderManager) QueryWithFallback(ctx context.Context, prompt string, fallbackSources []struct {
	ProviderName string
	ModelName    string
}) (string, error) {
	if len(fallbackSources) == 0 {
		return "", fmt.Errorf("no fallback sources provided")
	}

	// Use a channel to get the first successful result
	resultChan := make(chan ParallelQueryResult, len(fallbackSources))
	var wg sync.WaitGroup

	// Start all queries concurrently
	for _, source := range fallbackSources {
		wg.Add(1)
		go func(providerName, modelName string) {
			defer wg.Done()
			start := time.Now()
			response, err := pm.QueryWithCache(ctx, providerName, modelName, prompt)
			duration := time.Since(start)

			resultChan <- ParallelQueryResult{
				ProviderName: providerName,
				ModelName:    modelName,
				Prompt:       prompt,
				Response:     response,
				Error:        err,
				Duration:     duration,
			}
		}(source.ProviderName, source.ModelName)
	}

	// Close the channel when all goroutines are done
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// Return the first successful result
	var errors []error
	for result := range resultChan {
		if result.Error == nil {
			pm.logger.Info(fmt.Sprintf("Successful query with %s/%s in %v",
				result.ProviderName, result.ModelName, result.Duration))
			return result.Response, nil
		}
		errors = append(errors, fmt.Errorf("%s/%s: %w",
			result.ProviderName, result.ModelName, result.Error))
	}

	// If all failed, return combined error
	if len(errors) > 0 {
		return "", fmt.Errorf("all providers failed: %v", errors)
	}

	return "", fmt.Errorf("no results received")
}

// PrewarmCache preloads cache with common queries in background
func (pm *ProviderManager) PrewarmCache(ctx context.Context, commonQueries []struct {
	ProviderName string
	ModelName    string
	Prompt       string
}) {
	if pm.cache == nil {
		pm.logger.Debug("Cache not available, skipping prewarm")
		return
	}

	go func() {
		pm.logger.Info(fmt.Sprintf("Prewarming cache with %d common queries", len(commonQueries)))

		// Process queries in smaller batches to avoid overwhelming the system
		batchSize := 3
		for i := 0; i < len(commonQueries); i += batchSize {
			end := i + batchSize
			if end > len(commonQueries) {
				end = len(commonQueries)
			}

			batch := commonQueries[i:end]
			results := pm.ParallelQuery(ctx, batch)

			// Log successful prewarm operations
			for _, result := range results {
				if result.Error == nil {
					pm.logger.Debug(fmt.Sprintf("Prewarmed cache: %s/%s",
						result.ProviderName, result.ModelName))
				}
			}

			// Small delay between batches to be respectful to AI providers
			time.Sleep(100 * time.Millisecond)
		}

		pm.logger.Info("Cache prewarming completed")
	}()
}

// Execution Management Methods

// EnableAutoExecution enables automatic command execution for all providers
func (pm *ProviderManager) EnableAutoExecution() {
	pm.executionConfig.AutoExecute = true
	pm.logger.Warn("Auto-execution enabled for all providers - commands will be executed automatically")

	// Update existing cached providers
	for providerName, provider := range pm.providers {
		if eap, ok := provider.(*ExecutionAwareProvider); ok {
			eap.EnableAutoExecution()
			pm.logger.Info(fmt.Sprintf("Auto-execution enabled for provider: %s", providerName))
		}
	}
}

// DisableAutoExecution disables automatic command execution for all providers
func (pm *ProviderManager) DisableAutoExecution() {
	pm.executionConfig.AutoExecute = false
	pm.logger.Info("Auto-execution disabled for all providers - commands will be suggested only")

	// Update existing cached providers
	for providerName, provider := range pm.providers {
		if eap, ok := provider.(*ExecutionAwareProvider); ok {
			eap.DisableAutoExecution()
			pm.logger.Info(fmt.Sprintf("Auto-execution disabled for provider: %s", providerName))
		}
	}
}

// SetExecutionEnabled enables or disables execution detection for all providers
func (pm *ProviderManager) SetExecutionEnabled(enabled bool) {
	pm.executionConfig.Enabled = enabled
	if enabled {
		pm.logger.Info("Execution detection enabled for all providers")
	} else {
		pm.logger.Info("Execution detection disabled for all providers")
	}

	// Clear provider cache to force reinitialization with new settings
	pm.RefreshProviders()
}

// IsExecutionEnabled returns whether execution detection is enabled
func (pm *ProviderManager) IsExecutionEnabled() bool {
	return pm.executionConfig.Enabled
}

// IsAutoExecuteEnabled returns whether auto-execution is enabled
func (pm *ProviderManager) IsAutoExecuteEnabled() bool {
	return pm.executionConfig.AutoExecute
}

// GetExecutionCapabilities returns execution capabilities for all providers
func (pm *ProviderManager) GetExecutionCapabilities() map[string]interface{} {
	capabilities := map[string]interface{}{
		"enabled":         pm.executionConfig.Enabled,
		"auto_execute":    pm.executionConfig.AutoExecute,
		"dry_run_default": pm.executionConfig.DryRunDefault,
		"pattern_count":   len(pm.executionConfig.Patterns),
		"providers":       make(map[string]interface{}),
	}

	// Get capabilities from each cached provider
	providerCaps := make(map[string]interface{})
	for providerName, provider := range pm.providers {
		if eap, ok := provider.(*ExecutionAwareProvider); ok {
			providerCaps[providerName] = eap.GetExecutionCapabilities()
		} else {
			providerCaps[providerName] = map[string]interface{}{
				"execution_aware": false,
			}
		}
	}
	capabilities["providers"] = providerCaps

	return capabilities
}

// getDefaultExecutionPatternsStrings returns default execution patterns as strings
func getDefaultExecutionPatternsStrings() []string {
	return []string{
		`(?i)\b(install|add|remove|delete|uninstall)\s+\w+`,
		`(?i)\b(rebuild|switch|build)\b`,
		`(?i)\b(run|execute|start|stop|restart)\s+\w+`,
		`(?i)\b(update|upgrade|download)\s+\w+`,
		`(?i)\b(enable|disable)\s+\w+`,
		`(?i)\b(check|status|list|show)\s+\w+`,
		`(?i)\bcan you (run|execute|install|build|start|stop)`,
		`(?i)\bplease (run|execute|install|build|start|stop)`,
		`(?i)\bhow do i (install|run|execute|build|start|stop)`,
	}
}
