package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"fmt"
	"net"
	"strings"
	"time"

	"github.com/linkerd/linkerd2/cli/flag"
	l5dcharts "github.com/linkerd/linkerd2/pkg/charts/linkerd2"
	"github.com/linkerd/linkerd2/pkg/cmd"
	flagspkg "github.com/linkerd/linkerd2/pkg/flags"
	"github.com/linkerd/linkerd2/pkg/inject"
	"github.com/linkerd/linkerd2/pkg/issuercerts"
	"github.com/linkerd/linkerd2/pkg/k8s"
	"github.com/linkerd/linkerd2/pkg/tls"
	"github.com/linkerd/linkerd2/pkg/version"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	k8sResource "k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

// makeInstallUpgradeFlags builds the set of flags which are used by install and
// upgrade.  These flags control the majority of how the control plane is
// configured.
func makeInstallUpgradeFlags(defaults *l5dcharts.Values) ([]flag.Flag, *pflag.FlagSet, error) {
	installUpgradeFlags := pflag.NewFlagSet("install", pflag.ExitOnError)

	issuanceLifetime, err := time.ParseDuration(defaults.Identity.Issuer.IssuanceLifetime)
	if err != nil {
		return nil, nil, err
	}
	clockSkewAllowance, err := time.ParseDuration(defaults.Identity.Issuer.ClockSkewAllowance)
	if err != nil {
		return nil, nil, err
	}

	flags := []flag.Flag{
		flag.NewBoolFlag(installUpgradeFlags, "linkerd-cni-enabled", defaults.CNIEnabled,
			"Omit the NET_ADMIN capability in the PSP and the proxy-init container when injecting the proxy; requires the linkerd-cni plugin to already be installed",
			func(values *l5dcharts.Values, value bool) error {
				values.CNIEnabled = value
				return nil
			}),

		flag.NewStringFlag(installUpgradeFlags, "controller-log-level", defaults.ControllerLogLevel,
			"Log level for the controller and web components", func(values *l5dcharts.Values, value string) error {
				values.ControllerLogLevel = value
				return nil
			}),

		// The HA flag must be processed before flags that set these values individually so that the
		// individual flags can override the HA defaults.
		flag.NewBoolFlag(installUpgradeFlags, "ha", false, "Enable HA deployment config for the control plane (default false)",
			func(values *l5dcharts.Values, value bool) error {
				values.HighAvailability = value
				if value {
					if err := l5dcharts.MergeHAValues(values); err != nil {
						return err
					}
				}
				return nil
			}),

		flag.NewUintFlag(installUpgradeFlags, "controller-replicas", defaults.ControllerReplicas,
			"Replicas of the controller to deploy", func(values *l5dcharts.Values, value uint) error {
				values.ControllerReplicas = value
				return nil
			}),

		flag.NewInt64Flag(installUpgradeFlags, "controller-uid", defaults.ControllerUID,
			"Run the control plane components under this user ID", func(values *l5dcharts.Values, value int64) error {
				values.ControllerUID = value
				return nil
			}),

		flag.NewInt64Flag(installUpgradeFlags, "controller-gid", defaults.ControllerGID,
			"Run the control plane components under this group ID", func(values *l5dcharts.Values, value int64) error {
				values.ControllerGID = value
				return nil
			}),

		flag.NewBoolFlag(installUpgradeFlags, "disable-h2-upgrade", !defaults.EnableH2Upgrade,
			"Prevents the controller from instructing proxies to perform transparent HTTP/2 upgrading (default false)",
			func(values *l5dcharts.Values, value bool) error {
				values.EnableH2Upgrade = !value
				return nil
			}),

		flag.NewBoolFlag(installUpgradeFlags, "disable-heartbeat", defaults.DisableHeartBeat,
			"Disables the heartbeat cronjob (default false)", func(values *l5dcharts.Values, value bool) error {
				values.DisableHeartBeat = value
				return nil
			}),

		flag.NewDurationFlag(installUpgradeFlags, "identity-issuance-lifetime", issuanceLifetime,
			"The amount of time for which the Identity issuer should certify identity",
			func(values *l5dcharts.Values, value time.Duration) error {
				values.Identity.Issuer.IssuanceLifetime = value.String()
				return nil
			}),

		flag.NewDurationFlag(installUpgradeFlags, "identity-clock-skew-allowance", clockSkewAllowance,
			"The amount of time to allow for clock skew within a Linkerd cluster",
			func(values *l5dcharts.Values, value time.Duration) error {
				values.Identity.Issuer.ClockSkewAllowance = value.String()
				return nil
			}),

		flag.NewBoolFlag(installUpgradeFlags, "control-plane-tracing", defaults.ControlPlaneTracing,
			"Enables Control Plane Tracing with the defaults", func(values *l5dcharts.Values, value bool) error {
				values.ControlPlaneTracing = value
				return nil
			}),

		flag.NewStringFlag(installUpgradeFlags, "control-plane-tracing-namespace", defaults.ControlPlaneTracingNamespace,
			"Send control plane traces to Linkerd-Jaeger extension in this namespace", func(values *l5dcharts.Values, value string) error {
				values.ControlPlaneTracingNamespace = value
				return nil
			}),

		flag.NewStringFlag(installUpgradeFlags, "identity-issuer-certificate-file", "",
			"A path to a PEM-encoded file containing the Linkerd Identity issuer certificate (generated by default)",
			func(values *l5dcharts.Values, value string) error {
				if value != "" {
					crt, err := loadCrtPEM(value)
					if err != nil {
						return err
					}
					values.Identity.Issuer.TLS.CrtPEM = crt
				}
				return nil
			}),

		flag.NewStringFlag(installUpgradeFlags, "identity-issuer-key-file", "",
			"A path to a PEM-encoded file containing the Linkerd Identity issuer private key (generated by default)",
			func(values *l5dcharts.Values, value string) error {
				if value != "" {
					key, err := loadKeyPEM(value)
					if err != nil {
						return err
					}
					values.Identity.Issuer.TLS.KeyPEM = key
				}
				return nil
			}),

		flag.NewStringFlag(installUpgradeFlags, "identity-trust-anchors-file", "",
			"A path to a PEM-encoded file containing Linkerd Identity trust anchors (generated by default)",
			func(values *l5dcharts.Values, value string) error {
				if value != "" {
					data, err := os.ReadFile(filepath.Clean(value))
					if err != nil {
						return err
					}
					values.IdentityTrustAnchorsPEM = string(data)
				}
				return nil
			}),

		flag.NewBoolFlag(installUpgradeFlags, "enable-endpoint-slices", defaults.EnableEndpointSlices,
			"Enables the usage of EndpointSlice informers and resources for destination service",
			func(values *l5dcharts.Values, value bool) error {
				values.EnableEndpointSlices = value
				return nil
			}),
	}

	// Hide developer focused flags in release builds.
	release, err := version.IsReleaseChannel(version.Version)
	if err != nil {
		log.Errorf("Unable to parse version: %s", version.Version)
	}
	if release {
		installUpgradeFlags.MarkHidden("control-plane-version")
	}
	installUpgradeFlags.MarkHidden("control-plane-tracing")
	installUpgradeFlags.MarkHidden("control-plane-tracing-namespace")

	return flags, installUpgradeFlags, nil
}

func loadCrtPEM(path string) (string, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", err
	}

	crt, err := tls.DecodePEMCrt(string(data))
	if err != nil {
		return "", err
	}
	return crt.EncodeCertificatePEM(), nil
}

func loadKeyPEM(path string) (string, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", err
	}

	key, err := tls.DecodePEMKey(string(data))
	if err != nil {
		return "", err
	}
	cred := tls.Cred{PrivateKey: key}
	return cred.EncodePrivateKeyPEM(), nil
}

// makeInstallFlags builds the set of flags which are used by install.  These
// flags should not be changed during an upgrade and are not available to the
// upgrade command.
func makeInstallFlags(defaults *l5dcharts.Values) ([]flag.Flag, *pflag.FlagSet) {

	installOnlyFlags := pflag.NewFlagSet("install-only", pflag.ExitOnError)

	flags := []flag.Flag{
		flag.NewStringFlag(installOnlyFlags, "cluster-domain", defaults.ClusterDomain,
			"Set custom cluster domain", func(values *l5dcharts.Values, value string) error {
				values.ClusterDomain = value
				return nil
			}),

		flag.NewStringFlag(installOnlyFlags, "identity-trust-domain", defaults.IdentityTrustDomain,
			"Configures the name suffix used for identities.", func(values *l5dcharts.Values, value string) error {
				values.IdentityTrustDomain = value
				return nil
			}),

		flag.NewBoolFlag(installOnlyFlags, "identity-external-issuer", false,
			"Whether to use an external identity issuer (default false)", func(values *l5dcharts.Values, value bool) error {
				if value {
					values.Identity.Issuer.Scheme = string(corev1.SecretTypeTLS)
				} else {
					values.Identity.Issuer.Scheme = k8s.IdentityIssuerSchemeLinkerd
				}
				return nil
			}),

		flag.NewBoolFlag(installOnlyFlags, "identity-external-ca", false,
			"Whether to use an external CA provider (default false)", func(values *l5dcharts.Values, value bool) error {
				values.Identity.ExternalCA = value
				return nil
			}),
	}

	return flags, installOnlyFlags
}

// makeProxyFlags builds the set of flags which affect how the proxy is
// configured.  These flags are available to the inject command and to the
// install and upgrade commands.
func makeProxyFlags(defaults *l5dcharts.Values) ([]flag.Flag, *pflag.FlagSet) {
	proxyFlags := pflag.NewFlagSet("proxy", pflag.ExitOnError)

	flags := []flag.Flag{
		flag.NewStringFlag(proxyFlags, "proxy-image", defaults.Proxy.Image.Name, "Linkerd proxy container image name",
			func(values *l5dcharts.Values, value string) error {
				values.Proxy.Image.Name = value
				return nil
			}),

		flag.NewStringFlag(proxyFlags, "init-image", defaults.ProxyInit.Image.Name, "Linkerd init container image name",
			func(values *l5dcharts.Values, value string) error {
				values.ProxyInit.Image.Name = value
				return nil
			}),

		flag.NewStringFlag(proxyFlags, "init-image-version", defaults.ProxyInit.Image.Version,
			"Linkerd init container image version", func(values *l5dcharts.Values, value string) error {
				values.ProxyInit.Image.Version = value
				return nil
			}),

		flag.NewStringFlag(proxyFlags, "image-pull-policy", defaults.ImagePullPolicy,
			"Docker image pull policy", func(values *l5dcharts.Values, value string) error {
				values.ImagePullPolicy = value
				values.Proxy.Image.PullPolicy = value
				values.ProxyInit.Image.PullPolicy = value
				values.DebugContainer.Image.PullPolicy = value
				return nil
			}),

		flag.NewUintFlag(proxyFlags, "inbound-port", uint(defaults.Proxy.Ports.Inbound),
			"Proxy port to use for inbound traffic", func(values *l5dcharts.Values, value uint) error {
				values.Proxy.Ports.Inbound = int32(value)
				return nil
			}),

		flag.NewUintFlag(proxyFlags, "outbound-port", uint(defaults.Proxy.Ports.Outbound),
			"Proxy port to use for outbound traffic", func(values *l5dcharts.Values, value uint) error {
				values.Proxy.Ports.Outbound = int32(value)
				return nil
			}),

		flag.NewStringSliceFlag(proxyFlags, "skip-inbound-ports", nil, "Ports and/or port ranges (inclusive) that should skip the proxy and send directly to the application",
			func(values *l5dcharts.Values, value []string) error {
				values.ProxyInit.IgnoreInboundPorts = strings.Join(value, ",")
				return nil
			}),

		flag.NewStringSliceFlag(proxyFlags, "skip-outbound-ports", nil, "Outbound ports and/or port ranges (inclusive) that should skip the proxy",
			func(values *l5dcharts.Values, value []string) error {
				values.ProxyInit.IgnoreOutboundPorts = strings.Join(value, ",")
				return nil
			}),

		flag.NewInt64Flag(proxyFlags, "proxy-uid", defaults.Proxy.UID, "Run the proxy under this user ID",
			func(values *l5dcharts.Values, value int64) error {
				values.Proxy.UID = value
				return nil
			}),

		flag.NewInt64Flag(proxyFlags, "proxy-gid", defaults.Proxy.GID, "Run the proxy under this group ID",
			func(values *l5dcharts.Values, value int64) error {
				values.Proxy.GID = value
				return nil
			}),

		flag.NewStringFlag(proxyFlags, "proxy-log-level", defaults.Proxy.LogLevel, "Log level for the proxy",
			func(values *l5dcharts.Values, value string) error {
				values.Proxy.LogLevel = value
				return nil
			}),

		flag.NewUintFlag(proxyFlags, "control-port", uint(defaults.Proxy.Ports.Control), "Proxy port to use for control",
			func(values *l5dcharts.Values, value uint) error {
				values.Proxy.Ports.Control = int32(value)
				return nil
			}),

		flag.NewUintFlag(proxyFlags, "admin-port", uint(defaults.Proxy.Ports.Admin), "Proxy port to serve metrics on",
			func(values *l5dcharts.Values, value uint) error {
				values.Proxy.Ports.Admin = int32(value)
				return nil
			}),

		flag.NewStringFlag(proxyFlags, "proxy-cpu-request", defaults.Proxy.Resources.CPU.Request, "Amount of CPU units that the proxy sidecar requests",
			func(values *l5dcharts.Values, value string) error {
				q, err := k8sResource.ParseQuantity(value)
				if err != nil {
					return err
				}
				c, err := inject.ToWholeCPUCores(q)
				if err != nil {
					return err
				}
				values.Proxy.Runtime.Workers.Minimum = c
				values.Proxy.Resources.CPU.Request = value
				return nil
			}),

		flag.NewStringFlag(proxyFlags, "proxy-cpu-limit", defaults.Proxy.Resources.CPU.Limit, "Maximum amount of CPU units that the proxy sidecar can use",
			func(values *l5dcharts.Values, value string) error {
				q, err := k8sResource.ParseQuantity(value)
				if err != nil {
					return err
				}
				c, err := inject.ToWholeCPUCores(q)
				if err != nil {
					return err
				}
				values.Proxy.Runtime.Workers.Maximum = c
				values.Proxy.Resources.CPU.Limit = value
				return nil
			}),

		flag.NewStringFlag(proxyFlags, "proxy-memory-request", defaults.Proxy.Resources.Memory.Request, "Amount of Memory that the proxy sidecar requests",
			func(values *l5dcharts.Values, value string) error {
				values.Proxy.Resources.Memory.Request = value
				return nil
			}),

		flag.NewStringFlag(proxyFlags, "proxy-memory-limit", defaults.Proxy.Resources.Memory.Limit, "Maximum amount of Memory that the proxy sidecar can use",
			func(values *l5dcharts.Values, value string) error {
				values.Proxy.Resources.Memory.Limit = value
				return nil
			}),

		flag.NewBoolFlag(proxyFlags, "enable-external-profiles", defaults.Proxy.EnableExternalProfiles, "Enable service profiles for non-Kubernetes services",
			func(values *l5dcharts.Values, value bool) error {
				values.Proxy.EnableExternalProfiles = value
				return nil
			}),

		flag.NewStringFlag(proxyFlags, "default-inbound-policy", defaults.Proxy.DefaultInboundPolicy, "Inbound policy to use to control inbound access to the proxy",
			func(values *l5dcharts.Values, value string) error {
				values.Proxy.DefaultInboundPolicy = value
				return nil
			}),

		// Deprecated flags

		flag.NewStringFlag(proxyFlags, "proxy-memory", defaults.Proxy.Resources.Memory.Request, "Amount of Memory that the proxy sidecar requests",
			func(values *l5dcharts.Values, value string) error {
				values.Proxy.Resources.Memory.Request = value
				return nil
			}),

		flag.NewStringFlag(proxyFlags, "proxy-cpu", defaults.Proxy.Resources.CPU.Request, "Amount of CPU units that the proxy sidecar requests",
			func(values *l5dcharts.Values, value string) error {
				values.Proxy.Resources.CPU.Request = value
				return nil
			}),

		flag.NewStringFlagP(proxyFlags, "proxy-version", "v", defaults.Proxy.Image.Version, "Tag to be used for the Linkerd proxy images",
			func(values *l5dcharts.Values, value string) error {
				values.Proxy.Image.Version = value
				return nil
			}),
	}

	registryFlag := flag.NewStringFlag(proxyFlags, "registry", cmd.DefaultDockerRegistry,
		fmt.Sprintf("Docker registry to pull images from ($%s)", flagspkg.EnvOverrideDockerRegistry),
		func(values *l5dcharts.Values, value string) error {
			values.ControllerImage = cmd.RegistryOverride(values.ControllerImage, value)
			values.PolicyController.Image.Name = cmd.RegistryOverride(values.PolicyController.Image.Name, value)
			values.DebugContainer.Image.Name = cmd.RegistryOverride(values.DebugContainer.Image.Name, value)
			values.Proxy.Image.Name = cmd.RegistryOverride(values.Proxy.Image.Name, value)
			values.ProxyInit.Image.Name = cmd.RegistryOverride(values.ProxyInit.Image.Name, value)
			return nil
		})
	if reg := os.Getenv(flagspkg.EnvOverrideDockerRegistry); reg != "" {
		registryFlag.Set(reg)
	}
	flags = append(flags, registryFlag)

	proxyFlags.MarkDeprecated("proxy-memory", "use --proxy-memory-request instead")
	proxyFlags.MarkDeprecated("proxy-cpu", "use --proxy-cpu-request instead")
	proxyFlags.MarkDeprecated("proxy-version", "use --set proxy.image.version=<version>")

	// Hide developer focused flags in release builds.
	release, err := version.IsReleaseChannel(version.Version)
	if err != nil {
		log.Errorf("Unable to parse version: %s", version.Version)
	}
	if release {
		proxyFlags.MarkHidden("proxy-image")
		proxyFlags.MarkHidden("proxy-version")
		proxyFlags.MarkHidden("image-pull-policy")
		proxyFlags.MarkHidden("init-image")
		proxyFlags.MarkHidden("init-image-version")
	}

	return flags, proxyFlags
}

// makeInjectFlags builds the set of flags which are exclusive to the inject
// command.  These flags configure the proxy but are not available to the
// install and upgrade commands.  This is generally for proxy configuration
// which is intended to be set on individual workloads rather than being
// cluster wide.
func makeInjectFlags(defaults *l5dcharts.Values) ([]flag.Flag, *pflag.FlagSet) {
	injectFlags := pflag.NewFlagSet("inject", pflag.ExitOnError)

	flags := []flag.Flag{
		flag.NewBoolFlag(injectFlags, "native-sidecar", false, "Enable native sidecar",
			func(values *l5dcharts.Values, value bool) error {
				values.Proxy.NativeSidecar = value
				return nil
			}),

		flag.NewInt64Flag(injectFlags, "wait-before-exit-seconds", int64(defaults.Proxy.WaitBeforeExitSeconds),
			"The period during which the proxy sidecar must stay alive while its pod is terminating. "+
				"Must be smaller than terminationGracePeriodSeconds for the pod (default 0)",
			func(values *l5dcharts.Values, value int64) error {
				values.Proxy.WaitBeforeExitSeconds = uint64(value)
				return nil
			}),

		flag.NewBoolFlag(injectFlags, "disable-identity", false,
			"Disables resources from participating in TLS identity", func(values *l5dcharts.Values, value bool) error {
				return errors.New("--disable-identity is no longer supported; identity is always required")
			}),

		flag.NewStringSliceFlag(injectFlags, "require-identity-on-inbound-ports", strings.Split(defaults.Proxy.RequireIdentityOnInboundPorts, ","),
			"Inbound ports on which the proxy should require identity", func(values *l5dcharts.Values, value []string) error {
				values.Proxy.RequireIdentityOnInboundPorts = strings.Join(value, ",")
				return nil
			}),

		flag.NewBoolFlag(injectFlags, "ingress", defaults.Proxy.IsIngress, "Enable ingress mode in the linkerd proxy",
			func(values *l5dcharts.Values, value bool) error {
				values.Proxy.IsIngress = value
				return nil
			}),

		flag.NewStringSliceFlag(injectFlags, "opaque-ports", strings.Split(defaults.Proxy.OpaquePorts, ","),
			"Set opaque ports on the proxy", func(values *l5dcharts.Values, value []string) error {
				values.Proxy.OpaquePorts = strings.Join(value, ",")
				return nil
			}),
	}
	injectFlags.MarkHidden("disable-identity")

	return flags, injectFlags
}

/* Validation */

func validateValues(ctx context.Context, k *k8s.KubernetesAPI, values *l5dcharts.Values) error {
	if !alphaNumDashDot.MatchString(values.LinkerdVersion) {
		return fmt.Errorf("%s is not a valid version", values.LinkerdVersion)
	}

	if _, err := log.ParseLevel(values.ControllerLogLevel); err != nil {
		return fmt.Errorf("--controller-log-level must be one of: panic, fatal, error, warn, info, debug, trace")
	}

	if values.Proxy.LogLevel == "" {
		return errors.New("--proxy-log-level must not be empty")
	}

	if values.EnableEndpointSlices && k != nil {
		err := k8s.EndpointSliceAccess(ctx, k)
		if err != nil {
			return err
		}
	}

	// Validate only if its not empty
	if values.IdentityTrustDomain != "" {
		if errs := validation.IsDNS1123Subdomain(values.IdentityTrustDomain); len(errs) > 0 {
			return fmt.Errorf("invalid trust domain '%s': %s", values.IdentityTrustDomain, errs[0])
		}
	}

	err := validateProxyValues(values)
	if err != nil {
		return err
	}

	if values.Identity.Issuer.Scheme == string(corev1.SecretTypeTLS) {
		if values.Identity.Issuer.TLS.CrtPEM != "" {
			return errors.New("--identity-issuer-certificate-file must not be specified if --identity-external-issuer=true")
		}
		if values.Identity.Issuer.TLS.KeyPEM != "" {
			return errors.New("--identity-issuer-key-file must not be specified if --identity-external-issuer=true")
		}
	}

	if values.Identity.Issuer.Scheme == string(corev1.SecretTypeTLS) && k != nil {
		externalIssuerData, err := issuercerts.FetchExternalIssuerData(ctx, k, controlPlaneNamespace)
		if err != nil {
			return err
		}
		_, err = externalIssuerData.VerifyAndBuildCreds()
		if err != nil {
			return fmt.Errorf("failed to validate issuer credentials: %w", err)
		}
	}

	if values.Identity.Issuer.Scheme == k8s.IdentityIssuerSchemeLinkerd {
		issuerData := issuercerts.IssuerCertData{
			IssuerCrt:    values.Identity.Issuer.TLS.CrtPEM,
			IssuerKey:    values.Identity.Issuer.TLS.KeyPEM,
			TrustAnchors: values.IdentityTrustAnchorsPEM,
		}
		_, err := issuerData.VerifyAndBuildCreds()
		if err != nil {
			return fmt.Errorf("failed to validate issuer credentials: %w", err)
		}
	}

	if err != nil {
		return err
	}

	return nil
}

func validateProxyValues(values *l5dcharts.Values) error {
	networks := strings.Split(values.ClusterNetworks, ",")
	for _, network := range networks {
		if _, _, err := net.ParseCIDR(network); err != nil {
			return fmt.Errorf("cannot parse destination get networks: %w", err)
		}
	}

	if values.Proxy.Image.Version != "" && !alphaNumDashDot.MatchString(values.Proxy.Image.Version) {
		return fmt.Errorf("%s is not a valid version", values.Proxy.Image.Version)
	}

	if !alphaNumDashDot.MatchString(values.ProxyInit.Image.Version) {
		return fmt.Errorf("%s is not a valid version", values.ProxyInit.Image.Version)
	}

	if values.ImagePullPolicy != "Always" && values.ImagePullPolicy != "IfNotPresent" && values.ImagePullPolicy != "Never" {
		return fmt.Errorf("--image-pull-policy must be one of: Always, IfNotPresent, Never")
	}

	if values.Proxy.Resources.CPU.Request != "" {
		if _, err := k8sResource.ParseQuantity(values.Proxy.Resources.CPU.Request); err != nil {
			return fmt.Errorf("Invalid cpu request '%s' for --proxy-cpu-request flag", values.Proxy.Resources.CPU.Request)
		}
	}

	if values.Proxy.Resources.Memory.Request != "" {
		if _, err := k8sResource.ParseQuantity(values.Proxy.Resources.Memory.Request); err != nil {
			return fmt.Errorf("Invalid memory request '%s' for --proxy-memory-request flag", values.Proxy.Resources.Memory.Request)
		}
	}

	if values.Proxy.Resources.CPU.Limit != "" {
		cpuLimit, err := k8sResource.ParseQuantity(values.Proxy.Resources.CPU.Limit)
		if err != nil {
			return fmt.Errorf("Invalid cpu limit '%s' for --proxy-cpu-limit flag", values.Proxy.Resources.CPU.Limit)
		}
		// Not checking for error because option proxyCPURequest was already validated
		if cpuRequest, _ := k8sResource.ParseQuantity(values.Proxy.Resources.CPU.Request); cpuRequest.MilliValue() > cpuLimit.MilliValue() {
			return fmt.Errorf("The cpu limit '%s' cannot be lower than the cpu request '%s'", values.Proxy.Resources.CPU.Limit, values.Proxy.Resources.CPU.Request)
		}
	}

	if values.Proxy.Resources.Memory.Limit != "" {
		memoryLimit, err := k8sResource.ParseQuantity(values.Proxy.Resources.Memory.Limit)
		if err != nil {
			return fmt.Errorf("Invalid memory limit '%s' for --proxy-memory-limit flag", values.Proxy.Resources.Memory.Limit)
		}
		// Not checking for error because option proxyMemoryRequest was already validated
		if memoryRequest, _ := k8sResource.ParseQuantity(values.Proxy.Resources.Memory.Request); memoryRequest.Value() > memoryLimit.Value() {
			return fmt.Errorf("The memory limit '%s' cannot be lower than the memory request '%s'", values.Proxy.Resources.Memory.Limit, values.Proxy.Resources.Memory.Request)
		}
	}

	if !validProxyLogLevel.MatchString(values.Proxy.LogLevel) {
		return fmt.Errorf("\"%s\" is not a valid proxy log level - for allowed syntax check https://docs.rs/env_logger/0.6.0/env_logger/#enabling-logging",
			values.Proxy.LogLevel)
	}

	if values.ProxyInit.IgnoreInboundPorts != "" {
		if err := validateRangeSlice(strings.Split(values.ProxyInit.IgnoreInboundPorts, ",")); err != nil {
			return err
		}
	}

	if values.ProxyInit.IgnoreOutboundPorts != "" {
		if err := validateRangeSlice(strings.Split(values.ProxyInit.IgnoreOutboundPorts, ",")); err != nil {
			return err
		}
	}

	if err := validatePolicy(values.Proxy.DefaultInboundPolicy); err != nil {
		return err
	}

	return nil
}

func validatePolicy(policy string) error {
	validPolicies := []string{"all-authenticated", "all-unauthenticated", "cluster-authenticated", "cluster-unauthenticated", "deny", "audit"}
	for _, p := range validPolicies {
		if p == policy {
			return nil
		}
	}
	return fmt.Errorf("--default-inbound-policy must be one of: %s (got %s)", strings.Join(validPolicies, ", "), policy)
}

// initializeIssuerCredentials populates the identity issuer TLS credentials.
// If we are using an externally managed issuer secret, all we need to do here
// is copy the trust root from the issuer secret.  Otherwise, if no credentials
// have already been supplied, we generate them.
func initializeIssuerCredentials(ctx context.Context, k *k8s.KubernetesAPI, values *l5dcharts.Values) error {
	if values.Identity.Issuer.Scheme == string(corev1.SecretTypeTLS) {
		// Using externally managed issuer credentials.  We need to copy the
		// trust root.
		if k == nil {
			return errors.New("--ignore-cluster is not supported when --identity-external-issuer=true")
		}
		externalIssuerData, err := issuercerts.FetchExternalIssuerData(ctx, k, controlPlaneNamespace)
		if err != nil {
			return err
		}
		values.IdentityTrustAnchorsPEM = externalIssuerData.TrustAnchors
	} else if values.Identity.Issuer.TLS.CrtPEM != "" || values.Identity.Issuer.TLS.KeyPEM != "" || values.IdentityTrustAnchorsPEM != "" {
		// If any credentials have already been supplied, check that they are
		// all present.
		if values.IdentityTrustAnchorsPEM == "" {
			return errors.New("a trust anchors file must be specified if other credentials are provided")
		}
		if values.Identity.Issuer.TLS.CrtPEM == "" {
			return errors.New("a certificate file must be specified if other credentials are provided")
		}
		if values.Identity.Issuer.TLS.KeyPEM == "" {
			return errors.New("a private key file must be specified if other credentials are provided")
		}
	} else {
		// No credentials have been supplied so we will generate them.
		root, err := tls.GenerateRootCAWithDefaults(issuerName(values.IdentityTrustDomain))
		if err != nil {
			return fmt.Errorf("failed to generate root certificate for identity: %w", err)
		}
		values.Identity.Issuer.TLS.KeyPEM = root.Cred.EncodePrivateKeyPEM()
		values.Identity.Issuer.TLS.CrtPEM = root.Cred.Crt.EncodeCertificatePEM()
		values.IdentityTrustAnchorsPEM = root.Cred.Crt.EncodeCertificatePEM()
	}
	return nil
}

func issuerName(trustDomain string) string {
	return fmt.Sprintf("identity.%s.%s", controlPlaneNamespace, trustDomain)
}
