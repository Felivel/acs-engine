package vlabs

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/acs-engine/pkg/api/common"
	"github.com/Azure/acs-engine/pkg/helpers"
	"github.com/Masterminds/semver"
	"github.com/satori/go.uuid"
	validator "gopkg.in/go-playground/validator.v9"
)

var (
	validate        *validator.Validate
	keyvaultIDRegex *regexp.Regexp
	labelValueRegex *regexp.Regexp
	labelKeyRegex   *regexp.Regexp
	// Any version has to be mirrored in https://acs-mirror.azureedge.net/github-coreos/etcd-v[Version]-linux-amd64.tar.gz
	etcdValidVersions = [...]string{"2.2.5", "2.3.0", "2.3.1", "2.3.2", "2.3.3", "2.3.4", "2.3.5", "2.3.6", "2.3.7", "2.3.8",
		"3.0.0", "3.0.1", "3.0.2", "3.0.3", "3.0.4", "3.0.5", "3.0.6", "3.0.7", "3.0.8", "3.0.9", "3.0.10", "3.0.11", "3.0.12", "3.0.13", "3.0.14", "3.0.15", "3.0.16", "3.0.17",
		"3.1.0", "3.1.1", "3.1.2", "3.1.2", "3.1.3", "3.1.4", "3.1.5", "3.1.6", "3.1.7", "3.1.8", "3.1.9", "3.1.10",
		"3.2.0", "3.2.1", "3.2.2", "3.2.3", "3.2.4", "3.2.5", "3.2.6", "3.2.7", "3.2.8", "3.2.9", "3.2.11", "3.2.12",
		"3.2.13", "3.2.14", "3.2.15", "3.2.16", "3.3.0", "3.3.1"}
	networkPluginPlusPolicyAllowed = []k8sNetworkConfig{
		{
			networkPlugin: "",
			networkPolicy: "",
		},
		{
			networkPlugin: "azure",
			networkPolicy: "",
		},
		{
			networkPlugin: "kubenet",
			networkPolicy: "",
		},
		{
			networkPlugin: "flannel",
			networkPolicy: "",
		},
		{
			networkPlugin: "cilium",
			networkPolicy: "",
		},
		{
			networkPlugin: "cilium",
			networkPolicy: "cilium",
		},
		{
			networkPlugin: "kubenet",
			networkPolicy: "calico",
		},
		{
			networkPlugin: "",
			networkPolicy: "calico",
		},
		{
			networkPlugin: "",
			networkPolicy: "cilium",
		},
		{
			networkPlugin: "",
			networkPolicy: "azure", // for backwards-compatibility w/ prior networkPolicy usage
		},
		{
			networkPlugin: "",
			networkPolicy: "none", // for backwards-compatibility w/ prior networkPolicy usage
		},
	}
)

const (
	labelKeyPrefixMaxLength = 253
	labelValueFormat        = "^([A-Za-z0-9][-A-Za-z0-9_.]{0,61})?[A-Za-z0-9]$"
	labelKeyFormat          = "^(([a-zA-Z0-9-]+[.])*[a-zA-Z0-9-]+[/])?([A-Za-z0-9][-A-Za-z0-9_.]{0,61})?[A-Za-z0-9]$"
)

type k8sNetworkConfig struct {
	networkPlugin string
	networkPolicy string
}

func init() {
	validate = validator.New()
	keyvaultIDRegex = regexp.MustCompile(`^/subscriptions/\S+/resourceGroups/\S+/providers/Microsoft.KeyVault/vaults/[^/\s]+$`)
	labelValueRegex = regexp.MustCompile(labelValueFormat)
	labelKeyRegex = regexp.MustCompile(labelKeyFormat)
}

func isValidEtcdVersion(etcdVersion string) error {
	// "" is a valid etcdVersion that maps to DefaultEtcdVersion
	if etcdVersion == "" {
		return nil
	}
	for _, ver := range etcdValidVersions {
		if ver == etcdVersion {
			return nil
		}
	}
	return fmt.Errorf("Invalid etcd version(%s), valid versions are%s", etcdVersion, etcdValidVersions)
}

// Validate implements APIObject
func (o *OrchestratorProfile) Validate(isUpdate bool) error {
	// Don't need to call validate.Struct(o)
	// It is handled by Properties.Validate()
	// On updates we only need to make sure there is a supported patch version for the minor version
	if !isUpdate {
		switch o.OrchestratorType {
		case DCOS:
			version := common.RationalizeReleaseAndVersion(
				o.OrchestratorType,
				o.OrchestratorRelease,
				o.OrchestratorVersion,
				false)
			if version == "" {
				return fmt.Errorf("the following user supplied OrchestratorProfile configuration is not supported: OrchestratorType: %s, OrchestratorRelease: %s, OrchestratorVersion: %s. Please check supported Release or Version for this build of acs-engine", o.OrchestratorType, o.OrchestratorRelease, o.OrchestratorVersion)
			}
			if o.DcosConfig != nil && o.DcosConfig.BootstrapProfile != nil {
				if len(o.DcosConfig.BootstrapProfile.StaticIP) > 0 {
					if net.ParseIP(o.DcosConfig.BootstrapProfile.StaticIP) == nil {
						return fmt.Errorf("DcosConfig.BootstrapProfile.StaticIP '%s' is an invalid IP address",
							o.DcosConfig.BootstrapProfile.StaticIP)
					}
				}
			}
		case Swarm:
		case SwarmMode:
		case Kubernetes:
			version := common.RationalizeReleaseAndVersion(
				o.OrchestratorType,
				o.OrchestratorRelease,
				o.OrchestratorVersion,
				false)
			if version == "" {
				return fmt.Errorf("the following user supplied OrchestratorProfile configuration is not supported: OrchestratorType: %s, OrchestratorRelease: %s, OrchestratorVersion: %s. Please check supported Release or Version for this build of acs-engine", o.OrchestratorType, o.OrchestratorRelease, o.OrchestratorVersion)
			}

			if o.KubernetesConfig != nil {
				err := o.KubernetesConfig.Validate(version)
				if err != nil {
					return err
				}
				minVersion := "1.7.0"

				if o.KubernetesConfig.EnableAggregatedAPIs {
					sv, err := semver.NewVersion(version)
					if err != nil {
						return fmt.Errorf("could not validate version %s", version)
					}
					cons, err := semver.NewConstraint("<" + minVersion)
					if err != nil {
						return fmt.Errorf("could not apply semver constraint < %s against version %s", minVersion, version)
					}
					if cons.Check(sv) {
						return fmt.Errorf("enableAggregatedAPIs is only available in Kubernetes version %s or greater; unable to validate for Kubernetes version %s",
							minVersion, version)
					}

					if o.KubernetesConfig.EnableRbac != nil {
						if !*o.KubernetesConfig.EnableRbac {
							return fmt.Errorf("enableAggregatedAPIs requires the enableRbac feature as a prerequisite")
						}
					}
				}

				if helpers.IsTrueBoolPointer(o.KubernetesConfig.EnableDataEncryptionAtRest) {
					sv, err := semver.NewVersion(version)
					if err != nil {
						return fmt.Errorf("could not validate version %s", version)
					}
					cons, err := semver.NewConstraint("<" + minVersion)
					if err != nil {
						return fmt.Errorf("could not apply semver constraint < %s against version %s", minVersion, version)
					}
					if cons.Check(sv) {
						return fmt.Errorf("enableDataEncryptionAtRest is only available in Kubernetes version %s or greater; unable to validate for Kubernetes version %s",
							minVersion, o.OrchestratorVersion)
					}
					if o.KubernetesConfig.EtcdEncryptionKey != "" {
						_, err = base64.URLEncoding.DecodeString(o.KubernetesConfig.EtcdEncryptionKey)
						if err != nil {
							return fmt.Errorf("etcdEncryptionKey must be base64 encoded. Please provide a valid base64 encoded value or leave the etcdEncryptionKey empty to auto-generate the value")
						}
					}
				}

				if helpers.IsTrueBoolPointer(o.KubernetesConfig.EnableEncryptionWithExternalKms) {
					sv, _ := semver.NewVersion(version)
					minVersion := "1.10.0"
					cons, _ := semver.NewConstraint("<" + minVersion)
					if cons.Check(sv) {
						return fmt.Errorf("enableEncryptionWithExternalKms is only available in Kubernetes version %s or greater; unable to validate for Kubernetes version %s",
							minVersion, o.OrchestratorVersion)
					}
				}

				if helpers.IsTrueBoolPointer(o.KubernetesConfig.EnablePodSecurityPolicy) {
					if !helpers.IsTrueBoolPointer(o.KubernetesConfig.EnableRbac) {
						return fmt.Errorf("enablePodSecurityPolicy requires the enableRbac feature as a prerequisite")
					}
					sv, err := semver.NewVersion(version)
					if err != nil {
						return fmt.Errorf("could not validate version %s", version)
					}
					minVersion := "1.8.0"
					cons, err := semver.NewConstraint("<" + minVersion)
					if err != nil {
						return fmt.Errorf("could not apply semver constraint < %s against version %s", minVersion, version)
					}
					if cons.Check(sv) {
						return fmt.Errorf("enablePodSecurityPolicy is only supported in acs-engine for Kubernetes version %s or greater; unable to validate for Kubernetes version %s",
							minVersion, version)
					}
				}
			}
		case OpenShift:
			// TODO: add appropriate additional validation logic
			if o.OrchestratorVersion != common.OpenShiftVersionUnstable {
				version := common.RationalizeReleaseAndVersion(
					o.OrchestratorType,
					o.OrchestratorRelease,
					o.OrchestratorVersion,
					false)
				if version == "" {
					return fmt.Errorf("OrchestratorProfile is not able to be rationalized, check supported Release or Version")
				}
			}
			if o.OpenShiftConfig == nil || o.OpenShiftConfig.ClusterUsername == "" || o.OpenShiftConfig.ClusterPassword == "" {
				return fmt.Errorf("ClusterUsername and ClusterPassword must both be specified")
			}
		default:
			return fmt.Errorf("OrchestratorProfile has unknown orchestrator: %s", o.OrchestratorType)
		}
	} else {
		switch o.OrchestratorType {
		case DCOS, Kubernetes:

			version := common.RationalizeReleaseAndVersion(
				o.OrchestratorType,
				o.OrchestratorRelease,
				o.OrchestratorVersion,
				false)
			if version == "" {
				patchVersion := common.GetValidPatchVersion(o.OrchestratorType, o.OrchestratorVersion)
				// if there isn't a supported patch version for this version fail
				if patchVersion == "" {
					return fmt.Errorf("the following user supplied OrchestratorProfile configuration is not supported: OrchestratorType: %s, OrchestratorRelease: %s, OrchestratorVersion: %s. Please check supported Release or Version for this build of acs-engine", o.OrchestratorType, o.OrchestratorRelease, o.OrchestratorVersion)
				}
			}

		}
	}

	if (o.OrchestratorType != Kubernetes && o.OrchestratorType != OpenShift) && o.KubernetesConfig != nil {
		return fmt.Errorf("KubernetesConfig can be specified only when OrchestratorType is Kubernetes or OpenShift")
	}

	if o.OrchestratorType != OpenShift && o.OpenShiftConfig != nil {
		return fmt.Errorf("OpenShiftConfig can be specified only when OrchestratorType is OpenShift")
	}

	if o.OrchestratorType != DCOS && o.DcosConfig != nil && (*o.DcosConfig != DcosConfig{}) {
		return fmt.Errorf("DcosConfig can be specified only when OrchestratorType is DCOS")
	}

	return nil
}

func validateImageNameAndGroup(name, resourceGroup string) error {
	if name == "" && resourceGroup != "" {
		return errors.New("imageName needs to be specified when imageResourceGroup is provided")
	}
	if name != "" && resourceGroup == "" {
		return errors.New("imageResourceGroup needs to be specified when imageName is provided")
	}
	return nil
}

// Validate implements APIObject
func (m *MasterProfile) Validate(o *OrchestratorProfile) error {
	if o.OrchestratorType == OpenShift && m.Count != 1 {
		return errors.New("openshift can only deployed with one master")
	}
	if m.ImageRef != nil {
		if err := validateImageNameAndGroup(m.ImageRef.Name, m.ImageRef.ResourceGroup); err != nil {
			return err
		}
	}
	return validateDNSName(m.DNSPrefix)
}

// Validate implements APIObject
func (a *AgentPoolProfile) Validate(orchestratorType string) error {
	// Don't need to call validate.Struct(a)
	// It is handled by Properties.Validate()
	if e := validatePoolName(a.Name); e != nil {
		return e
	}

	if e := validatePoolOSType(a.OSType); e != nil {
		return e
	}

	// for Kubernetes, we don't support AgentPoolProfile.DNSPrefix
	if orchestratorType == Kubernetes {
		if e := validate.Var(a.DNSPrefix, "len=0"); e != nil {
			return fmt.Errorf("AgentPoolProfile.DNSPrefix must be empty for Kubernetes")
		}
		if e := validate.Var(a.Ports, "len=0"); e != nil {
			return fmt.Errorf("AgentPoolProfile.Ports must be empty for Kubernetes")
		}
	}

	if a.DNSPrefix != "" {
		if e := validateDNSName(a.DNSPrefix); e != nil {
			return e
		}
		if len(a.Ports) > 0 {
			if e := validateUniquePorts(a.Ports, a.Name); e != nil {
				return e
			}
		} else {
			a.Ports = []int{80, 443, 8080}
		}
	} else {
		if e := validate.Var(a.Ports, "len=0"); e != nil {
			return fmt.Errorf("AgentPoolProfile.Ports must be empty when AgentPoolProfile.DNSPrefix is empty for Orchestrator: %s", string(orchestratorType))
		}
	}

	if len(a.DiskSizesGB) > 0 {
		if e := validate.Var(a.StorageProfile, "eq=StorageAccount|eq=ManagedDisks"); e != nil {
			return fmt.Errorf("property 'StorageProfile' must be set to either '%s' or '%s' when attaching disks", StorageAccount, ManagedDisks)
		}
		if e := validate.Var(a.AvailabilityProfile, "eq=VirtualMachineScaleSets|eq=AvailabilitySet"); e != nil {
			return fmt.Errorf("property 'AvailabilityProfile' must be set to either '%s' or '%s' when attaching disks", VirtualMachineScaleSets, AvailabilitySet)
		}
		if a.StorageProfile == StorageAccount && (a.AvailabilityProfile == VirtualMachineScaleSets) {
			return fmt.Errorf("VirtualMachineScaleSets does not support storage account attached disks.  Instead specify 'StorageAccount': '%s' or specify AvailabilityProfile '%s'", ManagedDisks, AvailabilitySet)
		}
	}
	if len(a.Ports) == 0 && len(a.DNSPrefix) > 0 {
		return fmt.Errorf("AgentPoolProfile.Ports must be non empty when AgentPoolProfile.DNSPrefix is specified")
	}
	if a.ImageRef != nil {
		return validateImageNameAndGroup(a.ImageRef.Name, a.ImageRef.ResourceGroup)
	}
	return nil
}

// Validate implements APIObject
func (o *OrchestratorVersionProfile) Validate() error {
	// The only difference compared with OrchestratorProfile.Validate is
	// Here we use strings.EqualFold, the other just string comparison.
	// Rationalize orchestrator type should be done from versioned to unversioned
	// I will go ahead to simplify this
	return o.OrchestratorProfile.Validate(false)
}

func validateKeyVaultSecrets(secrets []KeyVaultSecrets, requireCertificateStore bool) error {
	for _, s := range secrets {
		if len(s.VaultCertificates) == 0 {
			return fmt.Errorf("Invalid KeyVaultSecrets must have no empty VaultCertificates")
		}
		if s.SourceVault == nil {
			return fmt.Errorf("missing SourceVault in KeyVaultSecrets")
		}
		if s.SourceVault.ID == "" {
			return fmt.Errorf("KeyVaultSecrets must have a SourceVault.ID")
		}
		for _, c := range s.VaultCertificates {
			if _, e := url.Parse(c.CertificateURL); e != nil {
				return fmt.Errorf("Certificate url was invalid. received error %s", e)
			}
			if e := validateName(c.CertificateStore, "KeyVaultCertificate.CertificateStore"); requireCertificateStore && e != nil {
				return fmt.Errorf("%s for certificates in a WindowsProfile", e)
			}
		}
	}
	return nil
}

// Validate implements APIObject
func (l *LinuxProfile) Validate() error {
	// Don't need to call validate.Struct(l)
	// It is handled by Properties.Validate()
	if e := validate.Var(l.SSH.PublicKeys[0].KeyData, "required"); e != nil {
		return fmt.Errorf("KeyData in LinuxProfile.SSH.PublicKeys cannot be empty string")
	}
	if e := validateKeyVaultSecrets(l.Secrets, false); e != nil {
		return e
	}
	return nil
}

func handleValidationErrors(e validator.ValidationErrors) error {
	// Override any version specific validation error message

	// common.HandleValidationErrors if the validation error message is general
	return common.HandleValidationErrors(e)
}

// Validate implements APIObject
func (w *WindowsProfile) Validate() error {
	if e := validate.Var(w.AdminUsername, "required"); e != nil {
		return fmt.Errorf("WindowsProfile.AdminUsername is required, when agent pool specifies windows")
	}
	if e := validate.Var(w.AdminPassword, "required"); e != nil {
		return fmt.Errorf("WindowsProfile.AdminPassword is required, when agent pool specifies windows")
	}
	if e := validateKeyVaultSecrets(w.Secrets, true); e != nil {
		return e
	}
	return nil
}

// Validate implements APIObject
func (profile *AADProfile) Validate() error {
	if _, err := uuid.FromString(profile.ClientAppID); err != nil {
		return fmt.Errorf("clientAppID '%v' is invalid", profile.ClientAppID)
	}
	if _, err := uuid.FromString(profile.ServerAppID); err != nil {
		return fmt.Errorf("serverAppID '%v' is invalid", profile.ServerAppID)
	}
	if len(profile.TenantID) > 0 {
		if _, err := uuid.FromString(profile.TenantID); err != nil {
			return fmt.Errorf("tenantID '%v' is invalid", profile.TenantID)
		}
	}
	if len(profile.AdminGroupID) > 0 {
		if _, err := uuid.FromString(profile.AdminGroupID); err != nil {
			return fmt.Errorf("adminGroupID '%v' is invalid", profile.AdminGroupID)
		}
	}
	return nil
}

// Validate implements APIObject
func (a *Properties) Validate(isUpdate bool) error {
	if e := validate.Struct(a); e != nil {
		return handleValidationErrors(e.(validator.ValidationErrors))
	}
	if e := a.OrchestratorProfile.Validate(isUpdate); e != nil {
		return e
	}
	if e := a.validateNetworkPlugin(); e != nil {
		return e
	}
	if e := a.validateNetworkPolicy(); e != nil {
		return e
	}
	if e := a.validateNetworkPluginPlusPolicy(); e != nil {
		return e
	}
	if e := a.validateContainerRuntime(); e != nil {
		return e
	}
	if e := a.validateAddons(); e != nil {
		return e
	}
	if e := a.MasterProfile.Validate(a.OrchestratorProfile); e != nil {
		return e
	}
	if e := validateUniqueProfileNames(a.AgentPoolProfiles); e != nil {
		return e
	}

	if a.OrchestratorProfile.OrchestratorType == Kubernetes {
		useManagedIdentity := (a.OrchestratorProfile.KubernetesConfig != nil &&
			a.OrchestratorProfile.KubernetesConfig.UseManagedIdentity)

		if !useManagedIdentity {
			if a.ServicePrincipalProfile == nil {
				return fmt.Errorf("ServicePrincipalProfile must be specified with Orchestrator %s", a.OrchestratorProfile.OrchestratorType)
			}
			if e := validate.Var(a.ServicePrincipalProfile.ClientID, "required"); e != nil {
				return fmt.Errorf("the service principal client ID must be specified with Orchestrator %s", a.OrchestratorProfile.OrchestratorType)
			}
			if (len(a.ServicePrincipalProfile.Secret) == 0 && a.ServicePrincipalProfile.KeyvaultSecretRef == nil) ||
				(len(a.ServicePrincipalProfile.Secret) != 0 && a.ServicePrincipalProfile.KeyvaultSecretRef != nil) {
				return fmt.Errorf("either the service principal client secret or keyvault secret reference must be specified with Orchestrator %s", a.OrchestratorProfile.OrchestratorType)
			}

			if a.OrchestratorProfile.KubernetesConfig != nil && helpers.IsTrueBoolPointer(a.OrchestratorProfile.KubernetesConfig.EnableEncryptionWithExternalKms) && len(a.ServicePrincipalProfile.ObjectID) == 0 {
				return fmt.Errorf("the service principal object ID must be specified with Orchestrator %s when enableEncryptionWithExternalKms is true", a.OrchestratorProfile.OrchestratorType)
			}

			if a.ServicePrincipalProfile.KeyvaultSecretRef != nil {
				if e := validate.Var(a.ServicePrincipalProfile.KeyvaultSecretRef.VaultID, "required"); e != nil {
					return fmt.Errorf("the Keyvault ID must be specified for the Service Principle with Orchestrator %s", a.OrchestratorProfile.OrchestratorType)
				}
				if e := validate.Var(a.ServicePrincipalProfile.KeyvaultSecretRef.SecretName, "required"); e != nil {
					return fmt.Errorf("the Keyvault Secret must be specified for the Service Principle with Orchestrator %s", a.OrchestratorProfile.OrchestratorType)
				}
				if !keyvaultIDRegex.MatchString(a.ServicePrincipalProfile.KeyvaultSecretRef.VaultID) {
					return fmt.Errorf("service principal client keyvault secret reference is of incorrect format")
				}
			}
		}
	}

	if a.OrchestratorProfile.OrchestratorType == OpenShift && a.MasterProfile.StorageProfile != ManagedDisks {
		return errors.New("OpenShift orchestrator supports only ManagedDisks")
	}

	for i, agentPoolProfile := range a.AgentPoolProfiles {
		if e := agentPoolProfile.Validate(a.OrchestratorProfile.OrchestratorType); e != nil {
			return e
		}
		switch agentPoolProfile.AvailabilityProfile {
		case AvailabilitySet:
		case VirtualMachineScaleSets:
		case "":
		default:
			{
				return fmt.Errorf("unknown availability profile type '%s' for agent pool '%s'.  Specify either %s, or %s", agentPoolProfile.AvailabilityProfile, agentPoolProfile.Name, AvailabilitySet, VirtualMachineScaleSets)
			}
		}

		if a.OrchestratorProfile.OrchestratorType == OpenShift && agentPoolProfile.AvailabilityProfile != AvailabilitySet {
			return fmt.Errorf("Only AvailabilityProfile: AvailabilitySet is supported for Orchestrator 'OpenShift'")
		}

		validRoles := []AgentPoolProfileRole{AgentPoolProfileRoleEmpty}
		if a.OrchestratorProfile.OrchestratorType == OpenShift {
			validRoles = append(validRoles, AgentPoolProfileRoleInfra)
		}
		var found bool
		for _, validRole := range validRoles {
			if agentPoolProfile.Role == validRole {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("Role %q is not supported for Orchestrator %s", agentPoolProfile.Role, a.OrchestratorProfile.OrchestratorType)
		}

		/* this switch statement is left to protect newly added orchestrators until they support Managed Disks*/
		if agentPoolProfile.StorageProfile == ManagedDisks {
			switch a.OrchestratorProfile.OrchestratorType {
			case DCOS:
			case Swarm:
			case Kubernetes:
			case OpenShift:
			case SwarmMode:
			default:
				return fmt.Errorf("HA volumes are currently unsupported for Orchestrator %s", a.OrchestratorProfile.OrchestratorType)
			}
		}

		if a.OrchestratorProfile.OrchestratorType == OpenShift && agentPoolProfile.StorageProfile != ManagedDisks {
			return errors.New("OpenShift orchestrator supports only ManagedDisks")
		}

		if len(agentPoolProfile.CustomNodeLabels) > 0 {
			switch a.OrchestratorProfile.OrchestratorType {
			case DCOS:
			case Kubernetes:
				for k, v := range agentPoolProfile.CustomNodeLabels {
					if e := validateKubernetesLabelKey(k); e != nil {
						return e
					}
					if e := validateKubernetesLabelValue(v); e != nil {
						return e
					}
				}
			default:
				return fmt.Errorf("Agent Type attributes are only supported for DCOS and Kubernetes")
			}
		}

		// validation for VMSS for Kubernetes
		if a.OrchestratorProfile.OrchestratorType == Kubernetes && (agentPoolProfile.AvailabilityProfile == VirtualMachineScaleSets || len(agentPoolProfile.AvailabilityProfile) == 0) {
			version := common.RationalizeReleaseAndVersion(
				a.OrchestratorProfile.OrchestratorType,
				a.OrchestratorProfile.OrchestratorRelease,
				a.OrchestratorProfile.OrchestratorVersion,
				false)
			if version == "" {
				return fmt.Errorf("the following user supplied OrchestratorProfile configuration is not supported: OrchestratorType: %s, OrchestratorRelease: %s, OrchestratorVersion: %s. Please check supported Release or Version for this build of acs-engine", a.OrchestratorProfile.OrchestratorType, a.OrchestratorProfile.OrchestratorRelease, a.OrchestratorProfile.OrchestratorVersion)
			}

			sv, err := semver.NewVersion(version)
			if err != nil {
				return fmt.Errorf("could not validate version %s", version)
			}
			minVersion := "1.10.0"
			cons, err := semver.NewConstraint("<" + minVersion)
			if err != nil {
				return fmt.Errorf("could not apply semver constraint < %s against version %s", minVersion, version)
			}
			if cons.Check(sv) {
				return fmt.Errorf("VirtualMachineScaleSets are only available in Kubernetes version %s or greater; unable to validate for Kubernetes version %s",
					minVersion, version)
			}
		}

		// validation for instanceMetadata using VMSS on Kubernetes
		if a.OrchestratorProfile.OrchestratorType == Kubernetes && (agentPoolProfile.AvailabilityProfile == VirtualMachineScaleSets || len(agentPoolProfile.AvailabilityProfile) == 0) {
			version := common.RationalizeReleaseAndVersion(
				a.OrchestratorProfile.OrchestratorType,
				a.OrchestratorProfile.OrchestratorRelease,
				a.OrchestratorProfile.OrchestratorVersion,
				false)
			if version == "" {
				return fmt.Errorf("the following user supplied OrchestratorProfile configuration is not supported: OrchestratorType: %s, OrchestratorRelease: %s, OrchestratorVersion: %s. Please check supported Release or Version for this build of acs-engine", a.OrchestratorProfile.OrchestratorType, a.OrchestratorProfile.OrchestratorRelease, a.OrchestratorProfile.OrchestratorVersion)
			}

			sv, err := semver.NewVersion(version)
			if err != nil {
				return fmt.Errorf("could not validate version %s", version)
			}
			minVersion := "1.10.2"
			cons, err := semver.NewConstraint("<" + minVersion)
			if err != nil {
				return fmt.Errorf("could not apply semver constraint < %s against version %s", minVersion, version)
			}
			if a.OrchestratorProfile.KubernetesConfig != nil && a.OrchestratorProfile.KubernetesConfig.UseInstanceMetadata != nil {
				if *a.OrchestratorProfile.KubernetesConfig.UseInstanceMetadata && cons.Check(sv) {
					return fmt.Errorf("VirtualMachineScaleSets with instance metadata is supported for Kubernetes version %s or greater. Please set \"useInstanceMetadata\": false in \"kubernetesConfig\"", minVersion)
				}
			} else {
				if cons.Check(sv) {
					return fmt.Errorf("VirtualMachineScaleSets with instance metadata is supported for Kubernetes version %s or greater. Please set \"useInstanceMetadata\": false in \"kubernetesConfig\"", minVersion)
				}
			}
		}

		if a.OrchestratorProfile.OrchestratorType == Kubernetes && (agentPoolProfile.AvailabilityProfile == VirtualMachineScaleSets || len(agentPoolProfile.AvailabilityProfile) == 0) && agentPoolProfile.StorageProfile == StorageAccount {
			return fmt.Errorf("VirtualMachineScaleSets does not support %s disks.  Please specify \"storageProfile\": \"%s\" (recommended) or \"availabilityProfile\": \"%s\"", StorageAccount, ManagedDisks, AvailabilitySet)
		}

		if a.OrchestratorProfile.OrchestratorType == Kubernetes {
			if i == 0 {
				continue
			}
			if a.AgentPoolProfiles[i].AvailabilityProfile != a.AgentPoolProfiles[0].AvailabilityProfile {
				return fmt.Errorf("mixed mode availability profiles are not allowed. Please set either VirtualMachineScaleSets or AvailabilitySet in availabilityProfile for all agent pools")
			}
		}

		if agentPoolProfile.OSType == Windows {
			switch a.OrchestratorProfile.OrchestratorType {
			case DCOS:
			case Swarm:
			case SwarmMode:
			case Kubernetes:
				var version string
				if a.HasWindows() {
					version = common.RationalizeReleaseAndVersion(
						a.OrchestratorProfile.OrchestratorType,
						a.OrchestratorProfile.OrchestratorRelease,
						a.OrchestratorProfile.OrchestratorVersion,
						true)
				} else {
					version = common.RationalizeReleaseAndVersion(
						a.OrchestratorProfile.OrchestratorType,
						a.OrchestratorProfile.OrchestratorRelease,
						a.OrchestratorProfile.OrchestratorVersion,
						false)
				}
				if version == "" {
					return fmt.Errorf("the following user supplied OrchestratorProfile configuration is not supported: OrchestratorType: %s, OrchestratorRelease: %s, OrchestratorVersion: %s. Please check supported Release or Version for this build of acs-engine", a.OrchestratorProfile.OrchestratorType, a.OrchestratorProfile.OrchestratorRelease, a.OrchestratorProfile.OrchestratorVersion)
				}
				if supported, ok := common.AllKubernetesWindowsSupportedVersions[version]; !ok || !supported {
					return fmt.Errorf("Orchestrator %s version %s does not support Windows", a.OrchestratorProfile.OrchestratorType, version)
				}
			default:
				return fmt.Errorf("Orchestrator %s does not support Windows", a.OrchestratorProfile.OrchestratorType)
			}
			if a.WindowsProfile != nil {
				if e := a.WindowsProfile.Validate(); e != nil {
					return e
				}
			} else {
				return fmt.Errorf("WindowsProfile is required when the cluster definition contains Windows agent pool(s)")
			}
		}
	}
	if e := a.LinuxProfile.Validate(); e != nil {
		return e
	}
	if e := validateVNET(a); e != nil {
		return e
	}

	if a.AADProfile != nil {
		if a.OrchestratorProfile.OrchestratorType != Kubernetes {
			return fmt.Errorf("'aadProfile' is only supported by orchestrator '%v'", Kubernetes)
		}
		if e := a.AADProfile.Validate(); e != nil {
			return e
		}
	}

	switch a.OrchestratorProfile.OrchestratorType {
	case OpenShift:
		if a.AzProfile == nil || a.AzProfile.Location == "" ||
			a.AzProfile.ResourceGroup == "" || a.AzProfile.SubscriptionID == "" ||
			a.AzProfile.TenantID == "" {
			return fmt.Errorf("'azProfile' must be supplied in full for orchestrator '%v'", OpenShift)
		}
	default:
		if a.AzProfile != nil {
			return fmt.Errorf("'azProfile' is only supported by orchestrator '%v'", OpenShift)
		}
	}

	for _, extension := range a.ExtensionProfiles {
		if extension.ExtensionParametersKeyVaultRef != nil {
			if e := validate.Var(extension.ExtensionParametersKeyVaultRef.VaultID, "required"); e != nil {
				return fmt.Errorf("the Keyvault ID must be specified for Extension %s", extension.Name)
			}
			if e := validate.Var(extension.ExtensionParametersKeyVaultRef.SecretName, "required"); e != nil {
				return fmt.Errorf("the Keyvault Secret must be specified for Extension %s", extension.Name)
			}
			if !keyvaultIDRegex.MatchString(extension.ExtensionParametersKeyVaultRef.VaultID) {
				return fmt.Errorf("Extension %s's keyvault secret reference is of incorrect format", extension.Name)
			}
		}
	}

	if a.WindowsProfile != nil && a.WindowsProfile.WindowsImageSourceURL != "" {
		if a.OrchestratorProfile.OrchestratorType != DCOS && a.OrchestratorProfile.OrchestratorType != Kubernetes {
			return fmt.Errorf("Windows Custom Images are only supported if the Orchestrator Type is DCOS or Kubernetes")
		}
	}

	return nil
}

// Validate validates the KubernetesConfig.
func (a *KubernetesConfig) Validate(k8sVersion string) error {
	// number of minimum retries allowed for kubelet to post node status
	const minKubeletRetries = 4
	// k8s versions that have cloudprovider backoff enabled
	var backoffEnabledVersions = common.AllKubernetesSupportedVersions
	// at present all supported versions allow for cloudprovider backoff
	// disallow backoff for future versions thusly:
	// for version := range []string{"1.11.0", "1.11.1", "1.11.2"} {
	//     backoffEnabledVersions[version] = false
	// }

	// k8s versions that have cloudprovider rate limiting enabled (currently identical with backoff enabled versions)
	ratelimitEnabledVersions := backoffEnabledVersions

	if a.ClusterSubnet != "" {
		_, subnet, err := net.ParseCIDR(a.ClusterSubnet)
		if err != nil {
			return fmt.Errorf("OrchestratorProfile.KubernetesConfig.ClusterSubnet '%s' is an invalid subnet", a.ClusterSubnet)
		}

		if a.NetworkPlugin == "azure" {
			ones, bits := subnet.Mask.Size()
			if bits-ones <= 8 {
				return fmt.Errorf("OrchestratorProfile.KubernetesConfig.ClusterSubnet '%s' must reserve at least 9 bits for nodes", a.ClusterSubnet)
			}
		}
	}

	if a.DockerBridgeSubnet != "" {
		_, _, err := net.ParseCIDR(a.DockerBridgeSubnet)
		if err != nil {
			return fmt.Errorf("OrchestratorProfile.KubernetesConfig.DockerBridgeSubnet '%s' is an invalid subnet", a.DockerBridgeSubnet)
		}
	}

	if a.MaxPods != 0 {
		if a.MaxPods < KubernetesMinMaxPods {
			return fmt.Errorf("OrchestratorProfile.KubernetesConfig.MaxPods '%v' must be at least %v", a.MaxPods, KubernetesMinMaxPods)
		}
	}

	if a.KubeletConfig != nil {
		if _, ok := a.KubeletConfig["--node-status-update-frequency"]; ok {
			val := a.KubeletConfig["--node-status-update-frequency"]
			_, err := time.ParseDuration(val)
			if err != nil {
				return fmt.Errorf("--node-status-update-frequency '%s' is not a valid duration", val)
			}
		}
	}

	if _, ok := a.ControllerManagerConfig["--node-monitor-grace-period"]; ok {
		_, err := time.ParseDuration(a.ControllerManagerConfig["--node-monitor-grace-period"])
		if err != nil {
			return fmt.Errorf("--node-monitor-grace-period '%s' is not a valid duration", a.ControllerManagerConfig["--node-monitor-grace-period"])
		}
	}

	if a.KubeletConfig != nil {
		if _, ok := a.KubeletConfig["--node-status-update-frequency"]; ok {
			if _, ok := a.ControllerManagerConfig["--node-monitor-grace-period"]; ok {
				nodeStatusUpdateFrequency, _ := time.ParseDuration(a.KubeletConfig["--node-status-update-frequency"])
				ctrlMgrNodeMonitorGracePeriod, _ := time.ParseDuration(a.ControllerManagerConfig["--node-monitor-grace-period"])
				kubeletRetries := ctrlMgrNodeMonitorGracePeriod.Seconds() / nodeStatusUpdateFrequency.Seconds()
				if kubeletRetries < minKubeletRetries {
					return fmt.Errorf("acs-engine requires that --node-monitor-grace-period(%f)s be larger than nodeStatusUpdateFrequency(%f)s by at least a factor of %d; ", ctrlMgrNodeMonitorGracePeriod.Seconds(), nodeStatusUpdateFrequency.Seconds(), minKubeletRetries)
				}
			}
		}
		if _, ok := a.KubeletConfig["--non-masquerade-cidr"]; ok {
			if _, _, err := net.ParseCIDR(a.KubeletConfig["--non-masquerade-cidr"]); err != nil {
				return fmt.Errorf("--non-masquerade-cidr kubelet config '%s' is an invalid CIDR string", a.KubeletConfig["--non-masquerade-cidr"])
			}
		}
	}

	if _, ok := a.ControllerManagerConfig["--pod-eviction-timeout"]; ok {
		_, err := time.ParseDuration(a.ControllerManagerConfig["--pod-eviction-timeout"])
		if err != nil {
			return fmt.Errorf("--pod-eviction-timeout '%s' is not a valid duration", a.ControllerManagerConfig["--pod-eviction-timeout"])
		}
	}

	if _, ok := a.ControllerManagerConfig["--route-reconciliation-period"]; ok {
		_, err := time.ParseDuration(a.ControllerManagerConfig["--route-reconciliation-period"])
		if err != nil {
			return fmt.Errorf("--route-reconciliation-period '%s' is not a valid duration", a.ControllerManagerConfig["--route-reconciliation-period"])
		}
	}

	if a.CloudProviderBackoff {
		if !backoffEnabledVersions[k8sVersion] {
			return fmt.Errorf("cloudprovider backoff functionality not available in kubernetes version %s", k8sVersion)
		}
	}

	if a.CloudProviderRateLimit {
		if !ratelimitEnabledVersions[k8sVersion] {
			return fmt.Errorf("cloudprovider rate limiting functionality not available in kubernetes version %s", k8sVersion)
		}
	}

	if a.DNSServiceIP != "" || a.ServiceCidr != "" {
		if a.DNSServiceIP == "" {
			return errors.New("OrchestratorProfile.KubernetesConfig.ServiceCidr must be specified when DNSServiceIP is")
		}
		if a.ServiceCidr == "" {
			return errors.New("OrchestratorProfile.KubernetesConfig.DNSServiceIP must be specified when ServiceCidr is")
		}

		dnsIP := net.ParseIP(a.DNSServiceIP)
		if dnsIP == nil {
			return fmt.Errorf("OrchestratorProfile.KubernetesConfig.DNSServiceIP '%s' is an invalid IP address", a.DNSServiceIP)
		}

		_, serviceCidr, err := net.ParseCIDR(a.ServiceCidr)
		if err != nil {
			return fmt.Errorf("OrchestratorProfile.KubernetesConfig.ServiceCidr '%s' is an invalid CIDR subnet", a.ServiceCidr)
		}

		// Finally validate that the DNS ip is within the subnet
		if !serviceCidr.Contains(dnsIP) {
			return fmt.Errorf("OrchestratorProfile.KubernetesConfig.DNSServiceIP '%s' is not within the ServiceCidr '%s'", a.DNSServiceIP, a.ServiceCidr)
		}

		// and that the DNS IP is _not_ the subnet broadcast address
		broadcast := common.IP4BroadcastAddress(serviceCidr)
		if dnsIP.Equal(broadcast) {
			return fmt.Errorf("OrchestratorProfile.KubernetesConfig.DNSServiceIP '%s' cannot be the broadcast address of ServiceCidr '%s'", a.DNSServiceIP, a.ServiceCidr)
		}

		// and that the DNS IP is _not_ the first IP in the service subnet
		firstServiceIP := common.CidrFirstIP(serviceCidr.IP)
		if firstServiceIP.Equal(dnsIP) {
			return fmt.Errorf("OrchestratorProfile.KubernetesConfig.DNSServiceIP '%s' cannot be the first IP of ServiceCidr '%s'", a.DNSServiceIP, a.ServiceCidr)
		}
	}

	// Validate that we have a valid etcd version
	if e := isValidEtcdVersion(a.EtcdVersion); e != nil {
		return e
	}

	if a.UseCloudControllerManager != nil && *a.UseCloudControllerManager || a.CustomCcmImage != "" {
		sv, _ := semver.NewVersion(k8sVersion)
		cons, _ := semver.NewConstraint("<" + "1.8.0")
		if cons.Check(sv) {
			return fmt.Errorf("OrchestratorProfile.KubernetesConfig.UseCloudControllerManager and OrchestratorProfile.KubernetesConfig.CustomCcmImage not available in kubernetes version %s", k8sVersion)
		}
	}

	return nil
}

func (a *Properties) validateNetworkPlugin() error {
	var networkPlugin string

	switch a.OrchestratorProfile.OrchestratorType {
	case Kubernetes:
		if a.OrchestratorProfile.KubernetesConfig != nil {
			networkPlugin = a.OrchestratorProfile.KubernetesConfig.NetworkPlugin
		}
	default:
		return nil
	}

	// Check NetworkPlugin has a valid value.
	valid := false
	for _, plugin := range NetworkPluginValues {
		if networkPlugin == plugin {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("unknown networkPlugin '%s' specified", networkPlugin)
	}

	return nil
}

func (a *Properties) validateNetworkPolicy() error {
	var networkPolicy string

	switch a.OrchestratorProfile.OrchestratorType {
	case Kubernetes:
		if a.OrchestratorProfile.KubernetesConfig != nil {
			networkPolicy = a.OrchestratorProfile.KubernetesConfig.NetworkPolicy
		}
	default:
		return nil
	}

	// Check NetworkPolicy has a valid value.
	valid := false
	for _, plugin := range NetworkPolicyValues {
		if networkPolicy == plugin {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("unknown networkPolicy '%s' specified", networkPolicy)
	}

	// Temporary safety check, to be removed when Windows support is added.
	if (networkPolicy == "calico" || networkPolicy == "cilium" || networkPolicy == "flannel") && a.HasWindows() {
		return fmt.Errorf("networkPolicy '%s' is not supporting windows agents", networkPolicy)
	}

	return nil
}

func (a *Properties) validateNetworkPluginPlusPolicy() error {
	var config k8sNetworkConfig

	if a.OrchestratorProfile.KubernetesConfig != nil {
		config.networkPlugin = a.OrchestratorProfile.KubernetesConfig.NetworkPlugin
	}
	if a.OrchestratorProfile.KubernetesConfig != nil {
		config.networkPolicy = a.OrchestratorProfile.KubernetesConfig.NetworkPolicy
	}

	for _, c := range networkPluginPlusPolicyAllowed {
		if c.networkPlugin == config.networkPlugin && c.networkPolicy == config.networkPolicy {
			return nil
		}
	}
	return fmt.Errorf("networkPolicy '%s' is not supported with networkPlugin '%s'", config.networkPolicy, config.networkPlugin)
}

func (a *Properties) validateContainerRuntime() error {
	var containerRuntime string

	switch a.OrchestratorProfile.OrchestratorType {
	case Kubernetes:
		if a.OrchestratorProfile.KubernetesConfig != nil {
			containerRuntime = a.OrchestratorProfile.KubernetesConfig.ContainerRuntime
		}
	default:
		return nil
	}

	// Check ContainerRuntime has a valid value.
	valid := false
	for _, runtime := range ContainerRuntimeValues {
		if containerRuntime == runtime {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("unknown containerRuntime %q specified", containerRuntime)
	}

	// Make sure we don't use clear containers on windows.
	if (containerRuntime == "clear-containers" || containerRuntime == "containerd") && a.HasWindows() {
		return fmt.Errorf("containerRuntime %q is not supporting windows agents", containerRuntime)
	}

	return nil
}

func (a *Properties) validateAddons() error {
	if a.OrchestratorProfile.KubernetesConfig != nil && a.OrchestratorProfile.KubernetesConfig.Addons != nil {
		var isAvailabilitySets bool

		for _, agentPool := range a.AgentPoolProfiles {
			if len(agentPool.AvailabilityProfile) == 0 || agentPool.IsAvailabilitySets() {
				isAvailabilitySets = true
			}
		}

		for _, addon := range a.OrchestratorProfile.KubernetesConfig.Addons {
			if addon.Name == "cluster-autoscaler" && *addon.Enabled && isAvailabilitySets {
				return fmt.Errorf("Cluster Autoscaler add-on can only be used with VirtualMachineScaleSets. Please specify \"availabilityProfile\": \"%s\"", VirtualMachineScaleSets)
			}
		}
	}
	return nil
}

func validateName(name string, label string) error {
	if name == "" {
		return fmt.Errorf("%s must be a non-empty value", label)
	}
	return nil
}

func validatePoolName(poolName string) error {
	// we will cap at length of 12 and all lowercase letters since this makes up the VMName
	poolNameRegex := `^([a-z][a-z0-9]{0,11})$`
	re, err := regexp.Compile(poolNameRegex)
	if err != nil {
		return err
	}
	submatches := re.FindStringSubmatch(poolName)
	if len(submatches) != 2 {
		return fmt.Errorf("pool name '%s' is invalid. A pool name must start with a lowercase letter, have max length of 12, and only have characters a-z0-9", poolName)
	}
	return nil
}

func validatePoolOSType(os OSType) error {
	if os != Linux && os != Windows && os != "" {
		return fmt.Errorf("AgentPoolProfile.osType must be either Linux or Windows")
	}
	return nil
}

func validateDNSName(dnsName string) error {
	dnsNameRegex := `^([A-Za-z][A-Za-z0-9-]{1,43}[A-Za-z0-9])$`
	re, err := regexp.Compile(dnsNameRegex)
	if err != nil {
		return err
	}
	if !re.MatchString(dnsName) {
		return fmt.Errorf("DNS name '%s' is invalid. The DNS name must contain between 3 and 45 characters.  The name can contain only letters, numbers, and hyphens.  The name must start with a letter and must end with a letter or a number (length was %d)", dnsName, len(dnsName))
	}
	return nil
}

func validateUniqueProfileNames(profiles []*AgentPoolProfile) error {
	profileNames := make(map[string]bool)
	for _, profile := range profiles {
		if _, ok := profileNames[profile.Name]; ok {
			return fmt.Errorf("profile name '%s' already exists, profile names must be unique across pools", profile.Name)
		}
		profileNames[profile.Name] = true
	}
	return nil
}

func validateUniquePorts(ports []int, name string) error {
	portMap := make(map[int]bool)
	for _, port := range ports {
		if _, ok := portMap[port]; ok {
			return fmt.Errorf("agent profile '%s' has duplicate port '%d', ports must be unique", name, port)
		}
		portMap[port] = true
	}
	return nil
}

func validateKubernetesLabelValue(v string) error {
	if !(len(v) == 0) && !labelValueRegex.MatchString(v) {
		return fmt.Errorf("Label value '%s' is invalid. Valid label values must be 63 characters or less and must be empty or begin and end with an alphanumeric character ([a-z0-9A-Z]) with dashes (-), underscores (_), dots (.), and alphanumerics between", v)
	}
	return nil
}

func validateKubernetesLabelKey(k string) error {
	if !labelKeyRegex.MatchString(k) {
		return fmt.Errorf("Label key '%s' is invalid. Valid label keys have two segments: an optional prefix and name, separated by a slash (/). The name segment is required and must be 63 characters or less, beginning and ending with an alphanumeric character ([a-z0-9A-Z]) with dashes (-), underscores (_), dots (.), and alphanumerics between. The prefix is optional. If specified, the prefix must be a DNS subdomain: a series of DNS labels separated by dots (.), not longer than 253 characters in total, followed by a slash (/)", k)
	}
	prefix := strings.Split(k, "/")
	if len(prefix) != 1 && len(prefix[0]) > labelKeyPrefixMaxLength {
		return fmt.Errorf("Label key prefix '%s' is invalid. If specified, the prefix must be no longer than 253 characters in total", k)
	}
	return nil
}

func validateVNET(a *Properties) error {
	isCustomVNET := a.MasterProfile.IsCustomVNET()
	for _, agentPool := range a.AgentPoolProfiles {
		if agentPool.IsCustomVNET() != isCustomVNET {
			return fmt.Errorf("Multiple VNET Subnet configurations specified.  The master profile and each agent pool profile must all specify a custom VNET Subnet, or none at all")
		}
	}
	if isCustomVNET {
		subscription, resourcegroup, vnetname, _, e := GetVNETSubnetIDComponents(a.MasterProfile.VnetSubnetID)
		if e != nil {
			return e
		}

		for _, agentPool := range a.AgentPoolProfiles {
			agentSubID, agentRG, agentVNET, _, err := GetVNETSubnetIDComponents(agentPool.VnetSubnetID)
			if err != nil {
				return err
			}
			if agentSubID != subscription ||
				agentRG != resourcegroup ||
				agentVNET != vnetname {
				return errors.New("Multiple VNETS specified.  The master profile and each agent pool must reference the same VNET (but it is ok to reference different subnets on that VNET)")
			}
		}

		masterFirstIP := net.ParseIP(a.MasterProfile.FirstConsecutiveStaticIP)
		if masterFirstIP == nil {
			return fmt.Errorf("MasterProfile.FirstConsecutiveStaticIP (with VNET Subnet specification) '%s' is an invalid IP address", a.MasterProfile.FirstConsecutiveStaticIP)
		}

		if a.MasterProfile.VnetCidr != "" {
			_, _, err := net.ParseCIDR(a.MasterProfile.VnetCidr)
			if err != nil {
				return fmt.Errorf("MasterProfile.VnetCidr '%s' contains invalid cidr notation", a.MasterProfile.VnetCidr)
			}
		}
	}
	return nil
}

// GetVNETSubnetIDComponents extract subscription, resourcegroup, vnetname, subnetname from the vnetSubnetID
func GetVNETSubnetIDComponents(vnetSubnetID string) (string, string, string, string, error) {
	vnetSubnetIDRegex := `^\/subscriptions\/([^\/]*)\/resourceGroups\/([^\/]*)\/providers\/Microsoft.Network\/virtualNetworks\/([^\/]*)\/subnets\/([^\/]*)$`
	re, err := regexp.Compile(vnetSubnetIDRegex)
	if err != nil {
		return "", "", "", "", err
	}
	submatches := re.FindStringSubmatch(vnetSubnetID)
	if len(submatches) != 4 {
		return "", "", "", "", err
	}
	return submatches[1], submatches[2], submatches[3], submatches[4], nil
}
