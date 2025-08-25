package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/kagent-dev/kagent/go/controller/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/internal/version"

	"github.com/abiosoft/ishell/v2"
	"github.com/briandowns/spinner"
	"github.com/kagent-dev/kagent/go/cli/internal/config"
)

const (
	ProfileMinimal = "minimal"
	ProfileDemo    = "demo"
)

var (
	Profiles = []string{ProfileMinimal, ProfileDemo}
)

// installChart installs or upgrades a Helm chart with the given parameters
func installChart(ctx context.Context, chartName string, namespace string, registry string, version string, setValues []string, valuesFile string) (string, error) {
	args := []string{
		"upgrade",
		"--install",
		chartName,
		registry + chartName,
		"--version",
		version,
		"--namespace",
		namespace,
		"--create-namespace",
		"--wait",
		"--history-max",
		"2",
		"--timeout",
		"5m",
	}

	// Add set values if any
	for _, setValue := range setValues {
		args = append(args, "--set", setValue)
	}

	if valuesFile != "" {
		args = append(args, "-f", valuesFile)
	}

	cmd := exec.CommandContext(ctx, "helm", args...)
	if byt, err := cmd.CombinedOutput(); err != nil {
		return string(byt), err
	}
	return "", nil
}

func InstallCmd(ctx context.Context, cfg *config.Config, profile string) *PortForward {
	if version.Version == "dev" {
		fmt.Fprintln(os.Stderr, "Installation requires released version of kagent")
		return nil
	}

	// get model provider from KAGENT_DEFAULT_MODEL_PROVIDER environment variable or use DefaultModelProvider
	modelProvider := GetModelProvider()

	// If model provider is openai, check if the API key is set
	apiKeyName := GetProviderAPIKey(modelProvider)
	apiKeyValue := os.Getenv(apiKeyName)

	if apiKeyName != "" && apiKeyValue == "" {
		fmt.Fprintf(os.Stderr, "%s is not set\n", apiKeyName)
		fmt.Fprintf(os.Stderr, "Please set the %s environment variable\n", apiKeyName)
		return nil
	}

	helmConfig := setupHelmConfig(modelProvider, apiKeyValue)

	// Validate and normalize profile input
	profile = strings.TrimSpace(profile)
	switch profile {
	case "":
		// default to minimal. no warning as this is the default
		profile = ProfileMinimal
	case ProfileDemo, ProfileMinimal:
		// valid, no change
	default:
		fmt.Fprintln(os.Stderr, "Invalid --profile value, defaulting to minimal")
		profile = ProfileMinimal
	}

	return install(ctx, cfg, helmConfig, profile, modelProvider)
}

func InteractiveInstallCmd(ctx context.Context, c *ishell.Context) *PortForward {
	if version.Version == "dev" {
		fmt.Fprintln(os.Stderr, "Installation requires released version of kagent")
		return nil
	}

	cfg := config.GetCfg(c)

	// get model provider from KAGENT_DEFAULT_MODEL_PROVIDER environment variable or use DefaultModelProvider
	modelProvider := GetModelProvider()

	//if model provider is openai, check if the api key is set
	apiKeyName := GetProviderAPIKey(modelProvider)
	apiKeyValue := os.Getenv(apiKeyName)

	if apiKeyName != "" && apiKeyValue == "" {
		fmt.Fprintf(os.Stderr, "%s is not set\n", apiKeyName)
		fmt.Fprintf(os.Stderr, "Please set the %s environment variable\n", apiKeyName)
		return nil
	}

	helmConfig := setupHelmConfig(modelProvider, apiKeyValue)

	// Add profile selection
	profileIdx := c.MultiChoice(Profiles, "Select a profile:")
	selectedProfile := Profiles[profileIdx]

	return install(ctx, cfg, helmConfig, selectedProfile, modelProvider)
}

// helmConfig is the config for the kagent chart
type helmConfig struct {
	registry string
	version  string
	values   []string
}

// setupHelmConfig sets up the helm config for the kagent chart
func setupHelmConfig(modelProvider v1alpha1.ModelProvider, apiKeyValue string) helmConfig {
	// Build Helm values
	helmProviderKey := GetModelProviderHelmValuesKey(modelProvider)
	values := []string{
		fmt.Sprintf("providers.default=%s", helmProviderKey),
		fmt.Sprintf("providers.%s.apiKey=%s", helmProviderKey, apiKeyValue),
	}

	//allow user to set the helm registry and version
	helmRegistry := GetEnvVarWithDefault(KAGENT_HELM_REPO, DefaultHelmOciRegistry)
	helmVersion := GetEnvVarWithDefault(KAGENT_HELM_VERSION, version.Version)
	helmExtraArgs := GetEnvVarWithDefault(KAGENT_HELM_EXTRA_ARGS, "")

	// split helmExtraArgs by "--set" to get additional values
	extraValues := strings.Split(helmExtraArgs, "--set")
	for _, hev := range extraValues {
		values = append(values, hev)
	}

	return helmConfig{
		registry: helmRegistry,
		version:  helmVersion,
		values:   values,
	}
}

// install installs kagent and kagent-crds using the helm config
func install(ctx context.Context, cfg *config.Config, helmConfig helmConfig, profile string, modelProvider v1alpha1.ModelProvider) *PortForward {
	// spinner for installation progress
	s := spinner.New(spinner.CharSets[35], 100*time.Millisecond)

	// First install kagent-crds
	s.Suffix = " Installing kagent-crds from " + helmConfig.registry
	defer s.Stop()
	s.Start()
	if output, err := installChart(ctx, "kagent-crds", cfg.Namespace, helmConfig.registry, helmConfig.version, nil, ""); err != nil {
		// Always stop the spinner before printing error messages
		s.Stop()

		// Check for various CRD existence scenarios, this is to be compatible with
		// original kagent installation that had CRDs installed together with the kagent chart
		if strings.Contains(output, "exists and cannot be imported into the current release") {
			fmt.Fprintln(os.Stderr, "Warning: CRDs exist but aren't managed by helm.")
			fmt.Fprintln(os.Stderr, "Run `uninstall` or delete them manually to")
			fmt.Fprintln(os.Stderr, "ensure they're fully managed on next install.")
			// Restart the spinner
			s.Start()
		} else {
			fmt.Fprintln(os.Stderr, "Error installing kagent-crds:", output)
			return nil
		}
	}

	// Update status
	// Removing api key(s) from printed values
	redactedValues := []string{}
	for _, value := range helmConfig.values {
		if strings.Contains(value, "apiKey") {
			// Split the value by "=" and replace the second part with "********"
			parts := strings.Split(value, "=")
			redactedValues = append(redactedValues, parts[0]+"=********")
		} else {
			redactedValues = append(redactedValues, value)
		}
	}

	s.Suffix = fmt.Sprintf(" Installing kagent [%s] Using %s:%s %v", modelProvider, helmConfig.registry, helmConfig.version, redactedValues)
	if output, err := installChart(ctx, "kagent", cfg.Namespace, helmConfig.registry, helmConfig.version, helmConfig.values, GetHelmProfileUrl(profile)); err != nil {
		// Always stop the spinner before printing error messages
		s.Stop()
		fmt.Fprintln(os.Stderr, "Error installing kagent:", output)
		return nil
	}

	// Stop the spinner completely before printing the success message
	s.Stop()
	fmt.Fprintln(os.Stdout, "kagent installed successfully")

	pf, err := NewPortForward(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error starting port-forward: %v\n", err)
		return nil
	}
	return pf
}

// deleteCRDs manually deletes Kubernetes CRDs for kagent
// This is a workaround for the fact that helm doesn't delete CRDs automatically
func deleteCRDs(ctx context.Context) error {
	crds := []string{
		"agents.kagent.dev",
		"modelconfigs.kagent.dev",
		"teams.kagent.dev",
		"toolservers.kagent.dev",
	}

	var deleteErrors []string

	for _, crd := range crds {
		deleteCmd := exec.CommandContext(ctx, "kubectl", "delete", "crd", crd)
		if out, err := deleteCmd.CombinedOutput(); err != nil {
			if !strings.Contains(string(out), "not found") {
				errMsg := fmt.Sprintf("Error deleting CRD %s: %s", crd, string(out))
				fmt.Fprintln(os.Stderr, errMsg)
				deleteErrors = append(deleteErrors, errMsg)
			}
		} else {
			fmt.Fprintf(os.Stdout, "Successfully deleted CRD %s\n", crd)
		}
	}

	if len(deleteErrors) > 0 {
		return fmt.Errorf("failed to delete some CRDs: %s", strings.Join(deleteErrors, "; "))
	}
	return nil
}

func UninstallCmd(ctx context.Context, cfg *config.Config) {
	s := spinner.New(spinner.CharSets[35], 100*time.Millisecond)

	// First uninstall kagent
	s.Suffix = " Uninstalling kagent"
	s.Start()

	args := []string{
		"uninstall",
		"kagent",
		"--namespace",
		cfg.Namespace,
	}
	cmd := exec.CommandContext(ctx, "helm", args...)

	if out, err := cmd.CombinedOutput(); err != nil {
		s.Stop()
		// Check if this is because kagent doesn't exist
		output := string(out)
		if strings.Contains(output, "not found") {
			fmt.Fprintln(os.Stderr, "Warning: kagent release not found, skipping uninstallation")
		} else {
			fmt.Fprintln(os.Stderr, "Error uninstalling kagent:", output)
			return
		}
	}

	// Then uninstall kagent-crds
	s.Suffix = " Uninstalling kagent-crds"

	args = []string{
		"uninstall",
		"kagent-crds",
		"--namespace",
		cfg.Namespace,
	}
	cmd = exec.CommandContext(ctx, "helm", args...)

	if out, err := cmd.CombinedOutput(); err != nil {
		s.Stop()
		// Check if this is because kagent-crds doesn't exist
		output := string(out)
		if strings.Contains(output, "not found") {
			fmt.Fprintln(os.Stderr, "Warning: kagent-crds release not found, try to delete crds directly")
			// delete the CRDs directly, this is a workaround for the fact that helm doesn't delete CRDs
			if err := deleteCRDs(ctx); err != nil {
				fmt.Fprintln(os.Stderr, "Error deleting CRDs:", err)
				return
			}
		} else {
			fmt.Fprintln(os.Stderr, "Error uninstalling kagent-crds:", output)
			return
		}
	}

	s.Stop()
	fmt.Fprintln(os.Stdout, "\nkagent uninstalled successfully")
}
