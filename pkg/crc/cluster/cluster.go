package cluster

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/code-ready/crc/pkg/crc/errors"
	"github.com/code-ready/crc/pkg/crc/logging"
	"github.com/code-ready/crc/pkg/crc/network"
	"github.com/code-ready/crc/pkg/crc/oc"
	"github.com/code-ready/crc/pkg/crc/ssh"
	"github.com/pborman/uuid"
)

func WaitForSSH(sshRunner *ssh.Runner) error {
	checkSSHConnectivity := func() error {
		_, err := sshRunner.Run("exit 0")
		if err != nil {
			return &errors.RetriableError{Err: err}
		}
		return nil
	}

	return errors.RetryAfter(60, checkSSHConnectivity, time.Second)
}

type CertExpiryState int

const (
	Unknown CertExpiryState = iota
	CertNotExpired
	CertExpired
)

// CheckCertsValidity checks if the cluster certs have expired or going to expire in next 7 days
func CheckCertsValidity(sshRunner *ssh.Runner) (CertExpiryState, error) {
	certExpiryDate, err := getcertExpiryDateFromVM(sshRunner)
	if err != nil {
		return Unknown, err
	}
	if time.Now().After(certExpiryDate) {
		return CertExpired, fmt.Errorf("Certs have expired, they were valid till: %s", certExpiryDate.Format(time.RFC822))
	}

	return CertNotExpired, nil
}

func getcertExpiryDateFromVM(sshRunner *ssh.Runner) (time.Time, error) {
	certExpiryDate := time.Time{}
	certExpiryDateCmd := `date --date="$(sudo openssl x509 -in /var/lib/kubelet/pki/kubelet-client-current.pem -noout -enddate | cut -d= -f 2)" --iso-8601=seconds`
	output, err := sshRunner.Run(certExpiryDateCmd)
	if err != nil {
		return certExpiryDate, err
	}
	certExpiryDate, err = time.Parse(time.RFC3339, strings.TrimSpace(output))
	if err != nil {
		return certExpiryDate, err
	}
	return certExpiryDate, nil
}

// Return size of disk, used space in bytes and the mountpoint
func GetRootPartitionUsage(sshRunner *ssh.Runner) (int64, int64, error) {
	cmd := "df -B1 --output=size,used,target /sysroot | tail -1"

	out, err := sshRunner.Run(cmd)

	if err != nil {
		return 0, 0, err
	}
	diskDetails := strings.Split(strings.TrimSpace(out), " ")
	diskSize, err := strconv.ParseInt(diskDetails[0], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	diskUsage, err := strconv.ParseInt(diskDetails[1], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	return diskSize, diskUsage, nil
}

func AddPullSecret(sshRunner *ssh.Runner, ocConfig oc.Config, pullSec string) error {
	if err := addPullSecretToInstanceDisk(sshRunner, pullSec); err != nil {
		return err
	}

	base64OfPullSec := base64.StdEncoding.EncodeToString([]byte(pullSec))
	cmdArgs := []string{"patch", "secret", "pull-secret", "-p",
		fmt.Sprintf(`'{"data":{".dockerconfigjson":"%s"}}'`, base64OfPullSec),
		"-n", "openshift-config", "--type", "merge"}

	if err := WaitForOpenshiftResource(ocConfig, "secret"); err != nil {
		return err
	}
	_, stderr, err := ocConfig.RunOcCommandPrivate(cmdArgs...)
	if err != nil {
		return fmt.Errorf("Failed to add Pull secret %v: %s", err, stderr)
	}
	return nil
}

func UpdateClusterID(ocConfig oc.Config) error {
	clusterID := uuid.New()
	cmdArgs := []string{"patch", "clusterversion", "version", "-p",
		fmt.Sprintf(`'{"spec":{"clusterID":"%s"}}'`, clusterID), "--type", "merge"}

	if err := WaitForOpenshiftResource(ocConfig, "clusterversion"); err != nil {
		return err
	}
	_, stderr, err := ocConfig.RunOcCommand(cmdArgs...)
	if err != nil {
		return fmt.Errorf("Failed to update cluster ID %v: %s", err, stderr)
	}

	return nil
}

func AddProxyConfigToCluster(ocConfig oc.Config, proxy *network.ProxyConfig) error {
	cmdArgs := []string{"patch", "proxy", "cluster", "-p",
		fmt.Sprintf(`'{"spec":{"httpProxy":"%s", "httpsProxy":"%s", "noProxy":"%s"}}'`, proxy.HTTPProxy, proxy.HTTPSProxy, proxy.GetNoProxyString()),
		"-n", "openshift-config", "--type", "merge"}

	if err := WaitForOpenshiftResource(ocConfig, "proxy"); err != nil {
		return err
	}
	if _, stderr, err := ocConfig.RunOcCommand(cmdArgs...); err != nil {
		return fmt.Errorf("Failed to add proxy details %v: %s", err, stderr)
	}
	return nil
}

// AddProxyToKubeletAndCriO adds the systemd drop-in proxy configuration file to the instance,
// both services (kubelet and crio) need to be restarted after this change.
// Since proxy operator is not able to make changes to in the kubelet/crio side,
// this is the job of machine config operator on the node and for crc this is not
// possible so we do need to put it here.
func AddProxyToKubeletAndCriO(sshRunner *ssh.Runner, proxy *network.ProxyConfig) error {
	proxyTemplate := `[Service]
Environment=HTTP_PROXY=%s
Environment=HTTPS_PROXY=%s
Environment=NO_PROXY=.cluster.local,.svc,10.128.0.0/14,172.30.0.0/16,%s`
	p := fmt.Sprintf(proxyTemplate, proxy.HTTPProxy, proxy.HTTPSProxy, proxy.GetNoProxyString())
	// This will create a systemd drop-in configuration for proxy (both for kubelet and crio services) on the VM.
	err := sshRunner.SetTextContentAsRoot("/etc/systemd/system/crio.service.d/10-default-env.conf", p, 0644)
	if err != nil {
		return err
	}
	err = sshRunner.SetTextContentAsRoot("/etc/systemd/system/kubelet.service.d/10-default-env.conf", p, 0644)
	if err != nil {
		return err
	}
	return nil
}

func addPullSecretToInstanceDisk(sshRunner *ssh.Runner, pullSec string) error {
	err := sshRunner.SetTextContentAsRoot("/var/lib/kubelet/config.json", pullSec, 0600)
	if err != nil {
		return err
	}

	return nil
}

func WaitforRequestHeaderClientCaFile(ocConfig oc.Config) error {
	if err := WaitForOpenshiftResource(ocConfig, "configmaps"); err != nil {
		return err
	}

	lookupRequestHeaderClientCa := func() error {
		cmdArgs := []string{"get", "configmaps/extension-apiserver-authentication", `-ojsonpath={.data.requestheader-client-ca-file}`,
			"-n", "kube-system"}

		stdout, stderr, err := ocConfig.RunOcCommand(cmdArgs...)
		if err != nil {
			return fmt.Errorf("Failed to get request header client ca file %v: %s", err, stderr)
		}
		if stdout == "" {
			return &errors.RetriableError{Err: fmt.Errorf("missing .data.requestheader-client-ca-file")}
		}
		logging.Debugf("Found .data.requestheader-client-ca-file: %s", stdout)
		return nil
	}
	return errors.RetryAfter(90, lookupRequestHeaderClientCa, 2*time.Second)
}

func DeleteOpenshiftAPIServerPods(ocConfig oc.Config) error {
	if err := WaitForOpenshiftResource(ocConfig, "pod"); err != nil {
		return err
	}

	deleteOpenshiftAPIServerPods := func() error {
		cmdArgs := []string{"delete", "pod", "--all", "-n", "openshift-apiserver"}
		_, _, err := ocConfig.RunOcCommand(cmdArgs...)
		if err != nil {
			return &errors.RetriableError{Err: err}
		}
		return nil
	}

	return errors.RetryAfter(60, deleteOpenshiftAPIServerPods, time.Second)
}

func CheckProxySettingsForOperator(ocConfig oc.Config, proxy *network.ProxyConfig, deployment, namespace string) (bool, error) {
	if !proxy.IsEnabled() {
		logging.Debugf("No proxy in use")
		return true, nil
	}
	cmdArgs := []string{"set", "env", "deployment", deployment, "--list", "-n", namespace}
	out, _, err := ocConfig.RunOcCommand(cmdArgs...)
	if err != nil {
		return false, err
	}
	if strings.Contains(out, proxy.HTTPSProxy) || strings.Contains(out, proxy.HTTPProxy) {
		return true, nil
	}
	return false, nil
}
