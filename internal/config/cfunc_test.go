package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validCfuncConfig() CfuncConfig {
	return CfuncConfig{
		Enabled:        true,
		RegistryPrefix: "registry.example/fission",
		MaxPoolSize:    10,
		DefaultSize:    "small",
		Sizes: map[string]ResourceSize{
			"small": {
				Requests: ResourceQuantities{CPU: "100m", Memory: "128Mi"},
				Limits:   ResourceQuantities{CPU: "200m", Memory: "256Mi"},
			},
			"large": {
				Requests: ResourceQuantities{CPU: "500m", Memory: "512Mi"},
				Limits:   ResourceQuantities{CPU: "1", Memory: "1Gi"},
			},
		},
		Flavors: map[string]CfuncFlavor{
			"python": {Image: "python-env:1.35.0", BuildImage: "python-builder:1.35.0"},
			"java":   {Image: "jvm-env:1.32.0", BuildImage: "jvm-builder:1.32.0", KeepArchive: true},
		},
		FetcherResources: ResourceSize{
			Requests: ResourceQuantities{CPU: "10m", Memory: "16Mi"},
			Limits:   ResourceQuantities{CPU: "200m", Memory: "128Mi"},
		},
		BuilderResources: ResourceSize{
			Requests: ResourceQuantities{CPU: "100m", Memory: "256Mi"},
			Limits:   ResourceQuantities{CPU: "1", Memory: "1Gi"},
		},
	}
}

func TestLoad_CfuncDefaults(t *testing.T) {
	cfg, err := Load("")
	require.NoError(t, err)

	assert.False(t, cfg.Resources.Cfunc.Enabled)
	assert.Equal(t, "ghcr.io/fission", cfg.Resources.Cfunc.RegistryPrefix)
	assert.False(t, cfg.Resources.Cfunc.AllowCustomBuild, "custom build images must be opt-in")
	assert.Equal(t, 10, cfg.Resources.Cfunc.MaxPoolSize)
	assert.Empty(t, cfg.Resources.Cfunc.Flavors)
	assert.Empty(t, cfg.Resources.Cfunc.Sizes)
	assert.Equal(t, ResourceSize{
		Requests: ResourceQuantities{CPU: "10m", Memory: "16Mi"},
		Limits:   ResourceQuantities{CPU: "200m", Memory: "128Mi"},
	}, cfg.Resources.Cfunc.FetcherResources)
	assert.Equal(t, ResourceSize{
		Requests: ResourceQuantities{CPU: "100m", Memory: "256Mi"},
		Limits:   ResourceQuantities{CPU: "1", Memory: "1Gi"},
	}, cfg.Resources.Cfunc.BuilderResources)
}

func TestLoad_CfuncFromFile(t *testing.T) {
	cfg := loadTempConfig(t, `
clusters:
  - name: dev
    credential:
      inline:
        apiServer: "https://dev.example.com:6443"
resources:
  cfunc:
    enabled: true
    registryPrefix: "registry.example/fission"
    imagePullSecret: "regcred"
    allowCustomBuild: true
    defaultSize: small
    sizes:
      small:
        requests: {cpu: "100m", memory: "128Mi"}
        limits: {cpu: "200m", memory: "256Mi"}
    flavors:
      Python:
        image: "python-env:1.35.0"
        buildImage: "python-builder:1.35.0"
        buildCommand: "build"
        builderEnv:
          - name: PIP_INDEX_URL
            value: "https://registry.example/pypi/simple"
      java:
        image: "jvm-env:1.32.0"
        keepArchive: true
`)
	env := cfg.Resources.Cfunc
	require.NoError(t, Validate(cfg))

	assert.Equal(t, "regcred", env.ImagePullSecret)
	assert.True(t, env.AllowCustomBuild)
	assert.Equal(t, []string{"small"}, env.SizeNames())
	assert.Equal(t, "100m", env.Sizes["small"].Requests.CPU)
	assert.Equal(t, "256Mi", env.Sizes["small"].Limits.Memory)

	// Viper lowercases map keys, so a flavor name is always lowercase.
	assert.Equal(t, []string{"java", "python"}, env.FlavorNames())
	python := env.Flavors["python"]
	assert.Equal(t, "python-builder:1.35.0", python.BuildImage)
	assert.Equal(t, "build", python.BuildCommand)
	// The variable name must keep its case, which is why builderEnv is a list.
	assert.Equal(t, []CfuncEnvVar{{Name: "PIP_INDEX_URL", Value: "https://registry.example/pypi/simple"}}, python.BuilderEnv)
	assert.True(t, env.Flavors["java"].KeepArchive)
}

func TestLoad_CfuncRegistryPrefixFromEnv(t *testing.T) {
	t.Setenv("CAPP_RESOURCES_CFUNC_REGISTRYPREFIX", "mirror.internal/fission")
	cfg := loadTempConfig(t, `
resources:
  cfunc:
    registryPrefix: "registry.example/fission"
`)
	assert.Equal(t, "mirror.internal/fission", cfg.Resources.Cfunc.RegistryPrefix)
}

func TestCfuncConfig_ImageRef(t *testing.T) {
	env := validCfuncConfig()

	assert.Equal(t, "registry.example/fission/python-env:1.35.0", env.ImageRef("python-env:1.35.0"))
	assert.Empty(t, env.ImageRef(""), "a flavor with no build image must not become a bare prefix")

	env.RegistryPrefix = ""
	assert.Equal(t, "python-env:1.35.0", env.ImageRef("python-env:1.35.0"))
}

func TestLoad_CfuncNamesWithADotAreRejected(t *testing.T) {
	for section, yaml := range map[string]string{
		"flavors": `
resources:
  cfunc:
    flavors:
      python3.11:
        image: "python-env:1.35.0"
`,
		"sizes": `
resources:
  cfunc:
    sizes:
      1.5x:
        requests: {cpu: "100m", memory: "128Mi"}
        limits: {cpu: "200m", memory: "256Mi"}
`,
	} {
		t.Run(section, func(t *testing.T) {
			_, err := Load(writeTempConfig(t, yaml))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "resources.cfunc."+section)
			assert.Contains(t, err.Error(), "must not contain a dot")
		})
	}
}

func TestValidate_Cfunc(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*CfuncConfig)
		errContain string
	}{
		{name: "valid"},
		{
			name:   "disabled — no validation",
			mutate: func(e *CfuncConfig) { *e = CfuncConfig{} },
		},
		{
			name:       "prefix with a scheme",
			mutate:     func(e *CfuncConfig) { e.RegistryPrefix = "https://registry.example" },
			errContain: "registryPrefix must be a registry path",
		},
		{
			name:       "prefix with a trailing slash",
			mutate:     func(e *CfuncConfig) { e.RegistryPrefix = "registry.example/" },
			errContain: "registryPrefix must be a registry path",
		},
		{
			name:       "invalid pull secret name",
			mutate:     func(e *CfuncConfig) { e.ImagePullSecret = "Not_A_Name" },
			errContain: "imagePullSecret",
		},
		{
			name:       "no sizes",
			mutate:     func(e *CfuncConfig) { e.Sizes = nil },
			errContain: "sizes must name at least one size",
		},
		{
			name:       "default size not configured",
			mutate:     func(e *CfuncConfig) { e.DefaultSize = "medium" },
			errContain: `defaultSize "medium" is not one of the configured sizes`,
		},
		{
			name:       "empty default size",
			mutate:     func(e *CfuncConfig) { e.DefaultSize = "" },
			errContain: "defaultSize",
		},
		{
			name: "size with an invalid quantity",
			mutate: func(e *CfuncConfig) {
				size := e.Sizes["small"]
				size.Limits.Memory = "lots"
				e.Sizes["small"] = size
			},
			errContain: `sizes.small: invalid limits.memory "lots"`,
		},
		{
			name: "size with a missing quantity",
			mutate: func(e *CfuncConfig) {
				size := e.Sizes["small"]
				size.Requests.CPU = ""
				e.Sizes["small"] = size
			},
			errContain: "sizes.small: invalid requests.cpu",
		},
		{
			name: "size request larger than its limit",
			mutate: func(e *CfuncConfig) {
				size := e.Sizes["small"]
				size.Requests.CPU = "2"
				e.Sizes["small"] = size
			},
			errContain: "sizes.small: requests.cpu is larger than limits.cpu",
		},
		{
			name: "two sizes with the same resources in different notation",
			mutate: func(e *CfuncConfig) {
				e.Sizes["huge"] = ResourceSize{
					Requests: ResourceQuantities{CPU: "0.5", Memory: "512Mi"},
					Limits:   ResourceQuantities{CPU: "1000m", Memory: "1024Mi"},
				}
			},
			errContain: `sizes.large has the same resources as size "huge"`,
		},
		{
			name:       "no flavors",
			mutate:     func(e *CfuncConfig) { e.Flavors = nil },
			errContain: "flavors must name at least one flavor",
		},
		{
			name:       "flavor without an image",
			mutate:     func(e *CfuncConfig) { e.Flavors["ruby"] = CfuncFlavor{BuildImage: "ruby-builder"} },
			errContain: "flavors.ruby.image is required",
		},
		{
			name:       "two flavors with the same image",
			mutate:     func(e *CfuncConfig) { e.Flavors["py"] = CfuncFlavor{Image: "python-env:1.35.0"} },
			errContain: `flavors.python has the same image as flavor "py"`,
		},
		{
			name: "size with a negative quantity",
			mutate: func(e *CfuncConfig) {
				size := e.Sizes["small"]
				size.Requests.CPU = "-100m"
				e.Sizes["small"] = size
			},
			errContain: `sizes.small: requests.cpu "-100m" must not be negative`,
		},
		{
			name:       "flavor with a build command and no build image",
			mutate:     func(e *CfuncConfig) { e.Flavors["perl"] = CfuncFlavor{Image: "perl-env:1.32.0", BuildCommand: "make"} },
			errContain: "flavors.perl.buildCommand needs a buildImage",
		},
		{
			name:       "max pool size of zero",
			mutate:     func(e *CfuncConfig) { e.MaxPoolSize = 0 },
			errContain: "maxPoolSize must be 1 or greater, got 0",
		},
		{
			name:       "flavor image with a typo",
			mutate:     func(e *CfuncConfig) { e.Flavors["ruby"] = CfuncFlavor{Image: "ruby-env: 1.32.0"} },
			errContain: `flavors.ruby.image "ruby-env: 1.32.0": registry.example/fission/ruby-env: 1.32.0 is not a valid image reference`,
		},
		{
			name: "build image with a typo",
			mutate: func(e *CfuncConfig) {
				e.Flavors["ruby"] = CfuncFlavor{Image: "ruby-env:1.32.0", BuildImage: "Ruby Builder"}
			},
			errContain: `flavors.ruby.buildImage "Ruby Builder"`,
		},
		{
			name: "flavor image that already names a registry",
			mutate: func(e *CfuncConfig) {
				e.Flavors["ruby"] = CfuncFlavor{Image: "ghcr.io/fission/ruby-env:1.32.0"}
			},
			errContain: "already names a registry (ghcr.io); with registryPrefix set, give the image relative to it",
		},
		{
			name: "flavor image with a path and no prefix",
			mutate: func(e *CfuncConfig) {
				e.RegistryPrefix = ""
				e.Flavors = map[string]CfuncFlavor{"ruby": {Image: "ghcr.io/fission/ruby-env:1.32.0"}}
			},
		},
		{
			name:   "flavor image in a sub-path of the prefix",
			mutate: func(e *CfuncConfig) { e.Flavors["ruby"] = CfuncFlavor{Image: "team/ruby-env:1.32.0"} },
		},
		{
			name:       "fetcher resources without a limit",
			mutate:     func(e *CfuncConfig) { e.FetcherResources.Limits.CPU = "" },
			errContain: "fetcherResources: invalid limits.cpu",
		},
		{
			name:       "builder request larger than its limit",
			mutate:     func(e *CfuncConfig) { e.BuilderResources.Requests.Memory = "2Gi" },
			errContain: "builderResources: requests.memory is larger than limits.memory",
		},
		{
			name: "builder variable without a name",
			mutate: func(e *CfuncConfig) {
				flavor := e.Flavors["python"]
				flavor.BuilderEnv = []CfuncEnvVar{{Value: "x"}}
				e.Flavors["python"] = flavor
			},
			errContain: "flavors.python.builderEnv[0].name is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := passthroughConfig()
			cfg.Resources.Cfunc = validCfuncConfig()
			if tt.mutate != nil {
				tt.mutate(&cfg.Resources.Cfunc)
			}
			err := Validate(cfg)
			if tt.errContain == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errContain)
		})
	}
}

func validCfuncFunctionsConfig() CfuncFunctionsConfig {
	return CfuncFunctionsConfig{
		Enabled:                true,
		Git:                    CfuncGitConfig{BaseURL: "https://git.example", Token: "s3cr3t-token", Username: "oauth2"},
		MaxTriggers:            10,
		MaxTimeoutSeconds:      300,
		DefaultConcurrency:     5,
		MaxConcurrency:         20,
		MinCronIntervalSeconds: 60,
	}
}

func TestLoad_CfuncFunctionsDefaults(t *testing.T) {
	cfg, err := Load("")
	require.NoError(t, err)

	functions := cfg.Resources.Cfunc.Functions
	assert.False(t, functions.Enabled)
	assert.False(t, cfg.Resources.Cfunc.OnboardNamespaces)
	assert.Equal(t, "oauth2", functions.Git.Username)
	assert.Equal(t, 10, functions.MaxTriggers)
	assert.Equal(t, 300, functions.MaxTimeoutSeconds)
	assert.Equal(t, 500, functions.DefaultConcurrency, "the Fission default")
	assert.Equal(t, 500, functions.MaxConcurrency)
	assert.Equal(t, 60, functions.MinCronIntervalSeconds)
}

func TestLoad_CfuncGitTokenFromEnv(t *testing.T) {
	t.Setenv("CAPP_RESOURCES_CFUNC_FUNCTIONS_GIT_TOKEN", "from-env")

	cfg, err := Load("")
	require.NoError(t, err)

	assert.Equal(t, "from-env", cfg.Resources.Cfunc.Functions.Git.Token)
}

func TestValidate_CfuncFunctions(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*CfuncConfig)
		errContain string
	}{
		{name: "valid"},
		{
			name:   "functions off — no validation",
			mutate: func(e *CfuncConfig) { e.Functions = CfuncFunctionsConfig{} },
		},
		{
			name:       "functions on while cfunc is off",
			mutate:     func(e *CfuncConfig) { e.Enabled = false },
			errContain: "functions.enabled needs",
		},
		{
			name:       "no base URL",
			mutate:     func(e *CfuncConfig) { e.Functions.Git.BaseURL = "" },
			errContain: "git.baseURL: is required",
		},
		{
			name:       "http base URL",
			mutate:     func(e *CfuncConfig) { e.Functions.Git.BaseURL = "http://git.example" },
			errContain: "must be an https URL",
		},
		{
			name:       "base URL with credentials",
			mutate:     func(e *CfuncConfig) { e.Functions.Git.BaseURL = "https://user:pw@git.example" },
			errContain: "must not carry credentials",
		},
		{
			name:       "no token",
			mutate:     func(e *CfuncConfig) { e.Functions.Git.Token = "" },
			errContain: "git.token is required",
		},
		{
			name:       "no username",
			mutate:     func(e *CfuncConfig) { e.Functions.Git.Username = "" },
			errContain: "git.username is required",
		},
		{
			name:       "zero trigger limit",
			mutate:     func(e *CfuncConfig) { e.Functions.MaxTriggers = 0 },
			errContain: "maxTriggers must be 1 or greater",
		},
		{
			name:       "zero timeout limit",
			mutate:     func(e *CfuncConfig) { e.Functions.MaxTimeoutSeconds = 0 },
			errContain: "maxTimeoutSeconds must be 1 or greater",
		},
		{
			name:       "zero cron interval",
			mutate:     func(e *CfuncConfig) { e.Functions.MinCronIntervalSeconds = 0 },
			errContain: "minCronIntervalSeconds must be 1 or greater",
		},
		{
			name:       "default concurrency above the maximum",
			mutate:     func(e *CfuncConfig) { e.Functions.DefaultConcurrency = 21 },
			errContain: "defaultConcurrency 21 is larger than maxConcurrency 20",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfunc := validCfuncConfig()
			cfunc.Functions = validCfuncFunctionsConfig()
			if tc.mutate != nil {
				tc.mutate(&cfunc)
			}

			errs := validateCfunc(&cfunc)

			if tc.errContain == "" {
				assert.Empty(t, errs)
				return
			}
			require.NotEmpty(t, errs)
			joined := ""
			for _, err := range errs {
				joined += err.Error() + "\n"
			}
			assert.Contains(t, joined, tc.errContain)
			assert.NotContains(t, joined, "s3cr3t-token", "a validation error must never print the git token")
		})
	}
}

func TestValidate_CfuncRouterURLs(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "unset", url: ""},
		{name: "https", url: "https://fn.example"},
		{name: "in-cluster http", url: "http://router.fission.svc"},
		{name: "no scheme", url: "fn.example", wantErr: true},
		{name: "other scheme", url: "ftp://fn.example", wantErr: true},
		{name: "credentials", url: "https://u:p@fn.example", wantErr: true},
		{name: "query", url: "https://fn.example?x=1", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateCfuncRouterURLs([]ClusterConfig{{Name: "prod", CfuncRouterURL: tc.url}})

			if tc.wantErr {
				require.Len(t, errs, 1)
				assert.Contains(t, errs[0].Error(), `clusters[0] ("prod"): cfuncRouterURL`)
				return
			}
			assert.Empty(t, errs)
		})
	}
}
