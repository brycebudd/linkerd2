package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/golang/protobuf/ptypes"
	pb "github.com/linkerd/linkerd2/controller/gen/config"
	charts "github.com/linkerd/linkerd2/pkg/charts/linkerd2"
	"github.com/linkerd/linkerd2/pkg/config"
	"github.com/linkerd/linkerd2/pkg/healthcheck"
	"github.com/linkerd/linkerd2/pkg/issuercerts"
	"github.com/linkerd/linkerd2/pkg/k8s"
	"github.com/linkerd/linkerd2/pkg/tls"
	"github.com/linkerd/linkerd2/pkg/version"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

const (
	controlPlaneMessage    = "Don't forget to run `linkerd upgrade control-plane`!"
	failMessage            = "For troubleshooting help, visit: https://linkerd.io/upgrade/#troubleshooting\n"
	trustRootChangeMessage = "Rotating the trust anchors will affect existing proxies\nSee https://linkerd.io/2/tasks/rotating_identity_certificates/ for more information"
)

type upgradeOptions struct {
	addOnOverwrite bool
	manifests      string
	force          bool
	*installOptions

	verifyTLS func(tls *charts.TLS, service string) error
}

func newUpgradeOptionsWithDefaults() (*upgradeOptions, error) {
	installOptions, err := newInstallOptionsWithDefaults()
	if err != nil {
		return nil, err
	}

	return &upgradeOptions{
		manifests:      "",
		installOptions: installOptions,
		verifyTLS:      verifyWebhookTLS,
	}, nil
}

// upgradeOnlyFlagSet includes flags that are only accessible at upgrade-time
// and not at install-time. also these flags are not intended to be persisted
// via linkerd-config ConfigMap, unlike recordableFlagSet
func (options *upgradeOptions) upgradeOnlyFlagSet() *pflag.FlagSet {
	flags := pflag.NewFlagSet("upgrade-only", pflag.ExitOnError)

	flags.StringVar(
		&options.manifests, "from-manifests", options.manifests,
		"Read config from a Linkerd install YAML rather than from Kubernetes",
	)
	flags.BoolVar(
		&options.force, "force", options.force,
		"Force upgrade operation even when issuer certificate does not work with the trust anchors of all proxies",
	)
	flags.BoolVar(
		&options.addOnOverwrite, "addon-overwrite", options.addOnOverwrite,
		"Overwrite (instead of merge) existing add-ons config with file in --addon-config (or reset to defaults if no new config is passed)",
	)
	return flags
}

// newCmdUpgradeConfig is a subcommand for `linkerd upgrade config`
func newCmdUpgradeConfig(options *upgradeOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config [flags]",
		Args:  cobra.NoArgs,
		Short: "Output Kubernetes cluster-wide resources to upgrade an existing Linkerd",
		Long: `Output Kubernetes cluster-wide resources to upgrade an existing Linkerd.

Note that this command should be followed by "linkerd upgrade control-plane".`,
		Example: `  # Default upgrade.
  linkerd upgrade config | kubectl apply -f -`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return upgradeRunE(cmd.Context(), options, configStage, options.recordableFlagSet())
		},
	}

	cmd.Flags().AddFlagSet(options.allStageFlagSet())

	return cmd
}

// newCmdUpgradeControlPlane is a subcommand for `linkerd upgrade control-plane`
func newCmdUpgradeControlPlane(options *upgradeOptions) *cobra.Command {
	flags := options.recordableFlagSet()

	cmd := &cobra.Command{
		Use:   "control-plane [flags]",
		Args:  cobra.NoArgs,
		Short: "Output Kubernetes control plane resources to upgrade an existing Linkerd",
		Long: `Output Kubernetes control plane resources to upgrade an existing Linkerd.

Note that the default flag values for this command come from the Linkerd control
plane. The default values displayed in the Flags section below only apply to the
install command. It should be run after "linkerd upgrade config".`,
		Example: `  # Default upgrade.
  linkerd upgrade control-plane | kubectl apply -f -`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return upgradeRunE(cmd.Context(), options, controlPlaneStage, flags)
		},
	}

	cmd.PersistentFlags().AddFlagSet(flags)

	return cmd
}

func newCmdUpgrade() *cobra.Command {
	options, err := newUpgradeOptionsWithDefaults()
	if err != nil {
		upgradeErrorf(err.Error())
	}

	flags := options.recordableFlagSet()
	upgradeOnlyFlags := options.upgradeOnlyFlagSet()

	cmd := &cobra.Command{
		Use:   "upgrade [flags]",
		Args:  cobra.NoArgs,
		Short: "Output Kubernetes configs to upgrade an existing Linkerd control plane",
		Long: `Output Kubernetes configs to upgrade an existing Linkerd control plane.

Note that the default flag values for this command come from the Linkerd control
plane. The default values displayed in the Flags section below only apply to the
install command.`,

		Example: `  # Default upgrade.
  linkerd upgrade | kubectl apply --prune -l linkerd.io/control-plane-ns=linkerd -f -

  # Similar to install, upgrade may also be broken up into two stages, by user
  # privilege.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return upgradeRunE(cmd.Context(), options, "", flags)
		},
	}

	cmd.Flags().AddFlagSet(flags)
	cmd.PersistentFlags().AddFlagSet(upgradeOnlyFlags)

	cmd.AddCommand(newCmdUpgradeConfig(options))
	cmd.AddCommand(newCmdUpgradeControlPlane(options))

	return cmd
}

func upgradeRunE(ctx context.Context, options *upgradeOptions, stage string, flags *pflag.FlagSet) error {
	if options.ignoreCluster {
		panic("ignore cluster must be unset") // Programmer error.
	}

	// We need a Kubernetes client to fetch configs and issuer secrets.
	var k *k8s.KubernetesAPI
	var err error
	if options.manifests != "" {
		readers, err := read(options.manifests)
		if err != nil {
			upgradeErrorf("Failed to parse manifests from %s: %s", options.manifests, err)
		}

		k, err = k8s.NewFakeAPIFromManifests(readers)
		if err != nil {
			upgradeErrorf("Failed to parse Kubernetes objects from manifest %s: %s", options.manifests, err)
		}
	} else {
		k, err = k8s.NewAPI(kubeconfigPath, kubeContext, impersonate, impersonateGroup, 0)
		if err != nil {
			upgradeErrorf("Failed to create a kubernetes client: %s", err)
		}
	}

	values, err := options.validateAndBuild(ctx, stage, k, flags)
	if err != nil {
		upgradeErrorf("Failed to build upgrade configuration: %s", err)
	}

	// rendering to a buffer and printing full contents of buffer after
	// render is complete, to ensure that okStatus prints separately
	var buf bytes.Buffer
	if err = render(&buf, values); err != nil {
		upgradeErrorf("Could not render upgrade configuration: %s", err)
	}

	if options.identityOptions.trustPEMFile != "" {
		fmt.Fprintf(os.Stderr, "\n%s %s\n\n", warnStatus, trustRootChangeMessage)
	}

	if stage == configStage {
		fmt.Fprintf(os.Stderr, "%s\n\n", controlPlaneMessage)
	}

	buf.WriteTo(os.Stdout)

	return nil
}

func (options *upgradeOptions) validateAndBuild(ctx context.Context, stage string, k *k8s.KubernetesAPI, flags *pflag.FlagSet) (*charts.Values, error) {
	if err := options.validate(); err != nil {
		return nil, err
	}

	// We fetch the configs directly from kubernetes because we need to be able
	// to upgrade/reinstall the control plane when the API is not available; and
	// this also serves as a passive check that we have privileges to access this
	// control plane.
	_, configs, err := healthcheck.FetchLinkerdConfigMap(ctx, k, controlPlaneNamespace)
	if err != nil {
		return nil, fmt.Errorf("could not fetch configs from kubernetes: %s", err)
	}

	// If the configs need to be repaired--either because sections did not
	// exist or because it is missing expected fields, repair it.
	repairConfigs(configs)

	// We recorded flags during a prior install. If we haven't overridden the
	// flag on this upgrade, reset that prior value as if it were specified now.
	//
	// This implies that the default flag values for the upgrade command come
	// from the control-plane, and not from the defaults specified in the FlagSet.
	setFlagsFromInstall(flags, configs.GetInstall().GetFlags())

	// Save off the updated set of flags into the installOptions so it gets
	// persisted with the upgraded config.
	options.recordFlags(flags)

	// Update the configs from the synthesized options.
	// The overrideConfigs() is used to override proxy configs only.
	options.overrideConfigs(configs, map[string]string{})

	if options.enableEndpointSlices {
		if err = validateEndpointSlicesFeature(ctx); err != nil {
			return nil, fmt.Errorf("--enableEndpointSlice=true not supported: %s", err)
		}
	}

	// Override configs with upgrade CLI options.
	if options.controlPlaneVersion != "" {
		configs.GetGlobal().Version = options.controlPlaneVersion
	}

	configs.GetInstall().Flags = options.recordedFlags

	configs.GetGlobal().OmitWebhookSideEffects = options.omitWebhookSideEffects
	if configs.GetGlobal().GetClusterDomain() == "" {
		configs.GetGlobal().ClusterDomain = defaultClusterDomain
	}

	if options.identityOptions.crtPEMFile != "" || options.identityOptions.keyPEMFile != "" {

		if configs.Global.IdentityContext.Scheme == string(corev1.SecretTypeTLS) {
			return nil, errors.New("cannot update issuer certificates if you are using external cert management solution")
		}

		if options.identityOptions.crtPEMFile == "" {
			return nil, errors.New("a certificate file must be specified if a private key is provided")
		}
		if options.identityOptions.keyPEMFile == "" {
			return nil, errors.New("a private key file must be specified if a certificate is provided")
		}
		if err := checkFilesExist([]string{options.identityOptions.crtPEMFile, options.identityOptions.keyPEMFile}); err != nil {
			return nil, err
		}
	}

	if options.identityOptions.trustPEMFile != "" {
		if err := checkFilesExist([]string{options.identityOptions.trustPEMFile}); err != nil {
			return nil, err
		}
	}

	var identity *identityWithAnchorsAndTrustDomain
	idctx := configs.GetGlobal().GetIdentityContext()
	if idctx.GetTrustDomain() == "" || idctx.GetTrustAnchorsPem() == "" {
		// If there wasn't an idctx, or if it doesn't specify the required fields, we
		// must be upgrading from a version that didn't support identity, so generate it anew...
		identity, err = options.identityOptions.genValues()
		if err != nil {
			return nil, err
		}
		configs.GetGlobal().IdentityContext = toIdentityContext(identity)
	} else {
		identity, err = options.fetchIdentityValues(ctx, k, idctx)
		if err != nil {
			return nil, err
		}
	}

	// Values have to be generated after any missing identity is generated,
	// otherwise it will be missing from the generated configmap.
	values, err := options.buildValuesWithoutIdentity(configs)
	if err != nil {
		return nil, fmt.Errorf("could not build install configuration: %s", err)
	}
	values.Identity = identity.Identity
	values.Global.IdentityTrustAnchorsPEM = identity.TrustAnchorsPEM
	values.Global.IdentityTrustDomain = identity.TrustDomain
	// we need to do that if we have updated the anchors as the config map json has already been generated
	if values.Global.IdentityTrustAnchorsPEM != configs.Global.IdentityContext.TrustAnchorsPem {
		// override the anchors in config
		configs.Global.IdentityContext.TrustAnchorsPem = values.Global.IdentityTrustAnchorsPEM
		// rebuild the json config map
		globalJSON, _, _, _ := config.ToJSON(configs)
		values.Configs.Global = globalJSON
	}

	// if exist, re-use the proxy injector, profile validator and tap TLS secrets.
	// otherwise, let Helm generate them by creating an empty charts.TLS struct here.
	proxyInjectorTLS, err := fetchTLSSecret(ctx, k, k8s.ProxyInjectorWebhookServiceName, options)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return nil, fmt.Errorf("could not fetch existing proxy injector secret: %s", err)
		}
		proxyInjectorTLS = &charts.TLS{}
	}
	values.ProxyInjector = &charts.ProxyInjector{TLS: proxyInjectorTLS}

	profileValidatorTLS, err := fetchTLSSecret(ctx, k, k8s.SPValidatorWebhookServiceName, options)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return nil, fmt.Errorf("could not fetch existing profile validator secret: %s", err)
		}
		profileValidatorTLS = &charts.TLS{}
	}
	values.ProfileValidator = &charts.ProfileValidator{TLS: profileValidatorTLS}

	tapTLS, err := fetchTLSSecret(ctx, k, k8s.TapServiceName, options)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return nil, fmt.Errorf("could not fetch existing tap secret: %s", err)
		}
		tapTLS = &charts.TLS{}
	}
	values.Tap = &charts.Tap{TLS: tapTLS}

	values.Stage = stage

	if !options.addOnOverwrite {
		// Update Add-Ons Configuration from the linkerd-value cm
		cmRawValues, _ := k8s.GetAddOnsConfigMap(ctx, k, controlPlaneNamespace)
		if cmRawValues != nil {
			//Cm is present now get the data
			cmData, ok := cmRawValues["values"]
			if !ok {
				return nil, fmt.Errorf("values subpath not found in %s configmap", k8s.AddOnsConfigMapName)
			}
			rawValues, err := yaml.Marshal(values)
			if err != nil {
				return nil, err
			}

			// over-write add-on values with cmValues
			// Merge Add-On Values with Values
			if rawValues, err = mergeRaw(rawValues, []byte(cmData)); err != nil {
				return nil, err
			}

			if err = yaml.Unmarshal(rawValues, &values); err != nil {
				return nil, err
			}
		}
	}

	// Update  add-ons Configuration  from config file
	// This allow users to over-write add-ons configuration during upgrades
	err = options.UpdateAddOnValuesFromConfig(values)
	if err != nil {
		return nil, err
	}

	return values, nil
}

func setFlagsFromInstall(flags *pflag.FlagSet, installFlags []*pb.Install_Flag) {
	for _, i := range installFlags {
		if f := flags.Lookup(i.GetName()); f != nil && !f.Changed {
			// The function recordFlags() stores the string representation of flags in the ConfigMap
			// so a stringSlice is stored e.g. as [a,b].
			// To avoid having f.Value.Set() interpreting that as a string we need to remove
			// the brackets
			value := i.GetValue()
			if f.Value.Type() == "stringSlice" {
				value = strings.Trim(value, "[]")
			}

			f.Value.Set(value)
			f.Changed = true
		}
	}
}

func repairConfigs(configs *pb.All) {
	// Repair the "install" section; install flags are updated separately
	if configs.Install == nil {
		configs.Install = &pb.Install{}
	}
	// ALWAYS update the CLI version to the most recent.
	configs.Install.CliVersion = version.Version

	// Repair the "proxy" section
	if configs.Proxy == nil {
		configs.Proxy = &pb.Proxy{}
	}
	if configs.Proxy.DebugImage == nil {
		configs.Proxy.DebugImage = &pb.Image{}
	}
	if configs.GetProxy().GetDebugImage().GetImageName() == "" {
		configs.Proxy.DebugImage.ImageName = k8s.DebugSidecarImage
	}
	if configs.GetProxy().GetDebugImageVersion() == "" {
		configs.Proxy.DebugImageVersion = version.Version
	}
}

func injectCABundle(ctx context.Context, k *k8s.KubernetesAPI, webhook string, value *charts.TLS) error {

	var err error

	switch webhook {
	case k8s.ProxyInjectorWebhookServiceName:
		err = injectCABundleFromMutatingWebhook(ctx, k, k8s.ProxyInjectorWebhookConfigName, value)
	case k8s.SPValidatorWebhookServiceName:
		err = injectCABundleFromValidatingWebhook(ctx, k, k8s.SPValidatorWebhookConfigName, value)
	case k8s.TapServiceName:
		err = injectCABundleFromAPIService(ctx, k, k8s.TapAPIRegistrationServiceName, value)
	default:
		err = fmt.Errorf("unknown webhook for retrieving CA bundle: %s", webhook)
	}

	// anything other than the resource not being found: propagate error back up the stack.
	if !kerrors.IsNotFound(err) {
		return err
	}

	// if the resource is missing, simply use the existing cert.
	value.CaBundle = value.CrtPEM
	return nil
}

func injectCABundleFromMutatingWebhook(ctx context.Context, k kubernetes.Interface, resource string, value *charts.TLS) error {
	webhookConf, err := k.AdmissionregistrationV1beta1().MutatingWebhookConfigurations().Get(ctx, resource, metav1.GetOptions{})
	if err != nil {
		return err
	}

	// note: this assumes that there will ever only be one service defined per webhook configuration
	value.CaBundle = string(webhookConf.Webhooks[0].ClientConfig.CABundle)

	return nil
}

func injectCABundleFromValidatingWebhook(ctx context.Context, k kubernetes.Interface, resource string, value *charts.TLS) error {
	webhookConf, err := k.AdmissionregistrationV1beta1().ValidatingWebhookConfigurations().Get(ctx, resource, metav1.GetOptions{})
	if err != nil {
		return err
	}

	value.CaBundle = string(webhookConf.Webhooks[0].ClientConfig.CABundle)
	return nil
}

func injectCABundleFromAPIService(ctx context.Context, k *k8s.KubernetesAPI, resource string, value *charts.TLS) error {
	apiService, err := k.Apiregistration.ApiregistrationV1().APIServices().Get(ctx, resource, metav1.GetOptions{})
	if err != nil {
		return err
	}

	value.CaBundle = string(apiService.Spec.CABundle)
	return nil
}

func fetchLinkerdTLSSecret(ctx context.Context, k *k8s.KubernetesAPI, webhook string, options *upgradeOptions) (*charts.TLS, error) {
	secret, err := k.CoreV1().
		Secrets(controlPlaneNamespace).
		Get(ctx, webhookSecretName(webhook), metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	value := &charts.TLS{
		KeyPEM: string(secret.Data["key.pem"]),
		CrtPEM: string(secret.Data["crt.pem"]),
	}

	if err := injectCABundle(ctx, k, webhook, value); err != nil {
		return nil, err
	}

	if err := options.verifyTLS(value, webhook); err != nil {
		return nil, err
	}

	return value, nil
}

func fetchK8sTLSSecret(ctx context.Context, k *k8s.KubernetesAPI, webhook string, options *upgradeOptions) (*charts.TLS, error) {
	webhookSecretNameStr := webhookK8sSecretName(webhook)

	secret, err := k.CoreV1().
		Secrets(controlPlaneNamespace).
		Get(ctx, webhookSecretNameStr, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	if secret.Data == nil || secret.Data["tls.key"] == nil || secret.Data["tls.crt"] == nil {
		return nil, fmt.Errorf("TLS Secret %s is missing data fields (data.tls.key and data.tls.crt expected)", webhookSecretNameStr)
	}

	value := &charts.TLS{
		KeyPEM: string(secret.Data["tls.key"]),
		CrtPEM: string(secret.Data["tls.crt"]),
	}

	if err := injectCABundle(ctx, k, webhook, value); err != nil {
		return nil, err
	}

	if err := options.verifyTLS(value, webhook); err != nil {
		return nil, err
	}

	return value, nil
}

// try to fetch K8sTLSSecret, but revert to LinkerdTLSSecret if the former fails
func fetchTLSSecret(ctx context.Context, k *k8s.KubernetesAPI, webhook string, options *upgradeOptions) (*charts.TLS, error) {
	tls, err := fetchK8sTLSSecret(ctx, k, webhook, options)
	if err == nil {
		return tls, err
	}

	if !kerrors.IsNotFound(err) {
		return tls, err
	}

	return fetchLinkerdTLSSecret(ctx, k, webhook, options)
}

func ensureIssuerCertWorksWithAllProxies(ctx context.Context, k kubernetes.Interface, cred *tls.Cred) error {
	meshedPods, err := healthcheck.GetMeshedPodsIdentityData(ctx, k, "")
	var problematicPods []string
	if err != nil {
		return err
	}
	for _, pod := range meshedPods {
		anchors, err := tls.DecodePEMCertPool(pod.Anchors)

		if anchors != nil {
			err = cred.Verify(anchors, "", time.Time{})
		}

		if err != nil {
			problematicPods = append(problematicPods, fmt.Sprintf("* %s/%s", pod.Namespace, pod.Name))
		}
	}

	if len(problematicPods) > 0 {
		errorMessageHeader := "You are attempting to use an issuer certificate which does not validate against the trust anchors of the following pods:"
		errorMessageFooter := "These pods do not have the current trust bundle and must be restarted.  Use the --force flag to proceed anyway (this will likely prevent those pods from sending or receiving traffic)."
		return fmt.Errorf("%s\n\t%s\n%s", errorMessageHeader, strings.Join(problematicPods, "\n\t"), errorMessageFooter)
	}
	return nil
}

// fetchIdentityValue checks the kubernetes API to fetch an existing
// linkerd identity configuration.
//
// This bypasses the public API so that we can access secrets and validate
// permissions.
func (options *upgradeOptions) fetchIdentityValues(ctx context.Context, k kubernetes.Interface, idctx *pb.IdentityContext) (*identityWithAnchorsAndTrustDomain, error) {
	if idctx == nil {
		return nil, nil
	}

	if idctx.Scheme == "" {
		// if this is empty, then we are upgrading from a version
		// that did not support issuer schemes. Just default to the
		// linkerd one.
		idctx.Scheme = k8s.IdentityIssuerSchemeLinkerd
	}

	var trustAnchorsPEM string
	var issuerData *issuercerts.IssuerCertData
	var err error

	if options.identityOptions.trustPEMFile != "" {
		trustb, err := ioutil.ReadFile(options.identityOptions.trustPEMFile)
		if err != nil {
			return nil, err
		}
		trustAnchorsPEM = string(trustb)
	} else {
		trustAnchorsPEM = idctx.GetTrustAnchorsPem()
	}

	updatingIssuerCert := options.identityOptions.crtPEMFile != "" && options.identityOptions.keyPEMFile != ""

	if updatingIssuerCert {
		issuerData, err = readIssuer(trustAnchorsPEM, options.identityOptions.crtPEMFile, options.identityOptions.keyPEMFile)
	} else {
		issuerData, err = fetchIssuer(ctx, k, trustAnchorsPEM, idctx.Scheme)
	}
	if err != nil {
		return nil, err
	}

	cred, err := issuerData.VerifyAndBuildCreds("")
	if err != nil {
		return nil, fmt.Errorf("issuer certificate does not work with the provided anchors: %s\nFor more information: https://linkerd.io/2/tasks/rotating_identity_certificates/", err)
	}
	issuerData.Expiry = &cred.Crt.Certificate.NotAfter

	if updatingIssuerCert && !options.force {
		if err := ensureIssuerCertWorksWithAllProxies(ctx, k, cred); err != nil {
			return nil, err
		}
	}

	clockSkewDuration, err := ptypes.Duration(idctx.GetClockSkewAllowance())
	if err != nil {
		return nil, fmt.Errorf("could not convert clock skew protobuf Duration format into golang Duration: %s", err)
	}

	issuanceLifetimeDuration, err := ptypes.Duration(idctx.GetIssuanceLifetime())
	if err != nil {
		return nil, fmt.Errorf("could not convert issuance Lifetime protobuf Duration format into golang Duration: %s", err)
	}

	return &identityWithAnchorsAndTrustDomain{
		TrustDomain:     idctx.GetTrustDomain(),
		TrustAnchorsPEM: trustAnchorsPEM,
		Identity: &charts.Identity{

			Issuer: &charts.Issuer{
				Scheme:              idctx.Scheme,
				ClockSkewAllowance:  clockSkewDuration.String(),
				IssuanceLifetime:    issuanceLifetimeDuration.String(),
				CrtExpiry:           *issuerData.Expiry,
				CrtExpiryAnnotation: k8s.IdentityIssuerExpiryAnnotation,
				TLS: &charts.IssuerTLS{
					KeyPEM: issuerData.IssuerKey,
					CrtPEM: issuerData.IssuerCrt,
				},
			},
		},
	}, nil

}

func readIssuer(trustPEM, issuerCrtPath, issuerKeyPath string) (*issuercerts.IssuerCertData, error) {
	key, crt, err := issuercerts.LoadIssuerCrtAndKeyFromFiles(issuerKeyPath, issuerCrtPath)
	if err != nil {
		return nil, err
	}
	issuerData := &issuercerts.IssuerCertData{
		TrustAnchors: trustPEM,
		IssuerCrt:    crt,
		IssuerKey:    key,
	}

	return issuerData, nil
}

func fetchIssuer(ctx context.Context, k kubernetes.Interface, trustPEM string, scheme string) (*issuercerts.IssuerCertData, error) {
	var (
		issuerData *issuercerts.IssuerCertData
		err        error
	)
	switch scheme {
	case string(corev1.SecretTypeTLS):
		issuerData, err = issuercerts.FetchExternalIssuerData(ctx, k, controlPlaneNamespace)
	default:
		issuerData, err = issuercerts.FetchIssuerData(ctx, k, trustPEM, controlPlaneNamespace)
		if issuerData != nil && issuerData.TrustAnchors != trustPEM {
			issuerData.TrustAnchors = trustPEM
		}
	}
	if err != nil {
		return nil, err
	}

	return issuerData, nil
}

// upgradeErrorf prints the error message and quits the upgrade process
func upgradeErrorf(format string, a ...interface{}) {
	template := fmt.Sprintf("%s %s\n%s\n", failStatus, format, failMessage)
	fmt.Fprintf(os.Stderr, template, a...)
	os.Exit(1)
}

func webhookCommonName(webhook string) string {
	return fmt.Sprintf("%s.%s.svc", webhook, controlPlaneNamespace)
}

func webhookSecretName(webhook string) string {
	return fmt.Sprintf("%s-tls", webhook)
}

func webhookK8sSecretName(webhook string) string {
	return fmt.Sprintf("%s-k8s-tls", webhook)
}

func verifyWebhookTLS(value *charts.TLS, webhook string) error {
	crt, err := tls.DecodePEMCrt(value.CrtPEM)
	if err != nil {
		return err
	}
	anchors := crt.CertPool()
	if err := crt.Verify(anchors, webhookCommonName(webhook), time.Time{}); err != nil {
		return err
	}

	return nil
}
