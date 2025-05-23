// Copyright 2022 MobiledgeX, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cloudcommon

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	dme "github.com/edgexr/edge-cloud-platform/api/distributed_match_engine"
	"github.com/edgexr/edge-cloud-platform/api/edgeproto"
	"github.com/edgexr/edge-cloud-platform/pkg/deploygen"
	"github.com/edgexr/edge-cloud-platform/pkg/log"
	"github.com/edgexr/edge-cloud-platform/pkg/util"
	yaml "github.com/mobiledgex/yaml/v2"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

var DeploymentTypeKubernetes = "kubernetes"
var DeploymentTypeVM = "vm"
var DeploymentTypeHelm = "helm"
var DeploymentTypeDocker = "docker"

var Download = "download"
var NoDownload = "nodownload"
var NoCreds = ""

var ValidAppDeployments = []string{
	DeploymentTypeKubernetes,
	DeploymentTypeVM,
	DeploymentTypeHelm,
	DeploymentTypeDocker,
}

var ValidCloudletDeployments = []string{
	DeploymentTypeDocker,
	DeploymentTypeKubernetes,
}

type DockerManifest struct {
	DockerComposeFiles []string
}

func IsValidDeploymentType(DeploymentType string, validDeployments []string) bool {
	for _, d := range validDeployments {
		if DeploymentType == d {
			return true
		}
	}
	return false
}

func IsValidDeploymentForImage(imageType edgeproto.ImageType, deployment string) bool {
	switch imageType {
	case edgeproto.ImageType_IMAGE_TYPE_DOCKER:
		if deployment == DeploymentTypeKubernetes || deployment == DeploymentTypeDocker {
			return true
		}
	case edgeproto.ImageType_IMAGE_TYPE_QCOW:
		fallthrough
	case edgeproto.ImageType_IMAGE_TYPE_OVF:
		fallthrough
	case edgeproto.ImageType_IMAGE_TYPE_OVA:
		if deployment == DeploymentTypeVM {
			return true
		}
	case edgeproto.ImageType_IMAGE_TYPE_HELM:
		if deployment == DeploymentTypeHelm {
			return true
		}
	}
	return false
}

func GetDockerDeployType(manifest string) string {
	if manifest == "" {
		return "docker"
	}
	if strings.HasSuffix(manifest, ".zip") {
		return "docker-compose-zip"
	}
	return "docker-compose"
}

// GetMappedAccessType gets the default access type for the deployment.  As of 2.4.1 only Load Balancer access is supported.  Once
// the UI is updated to remove all references to access type, this can be removed altogether
func GetMappedAccessType(accessType edgeproto.AccessType, deployment, deploymentManifest string) (edgeproto.AccessType, error) {
	if accessType == edgeproto.AccessType_ACCESS_TYPE_DIRECT {
		// this can be removed altogether once removed from the UI
		return accessType, fmt.Errorf("Access Type Direct no longer supported")
	}
	return edgeproto.AccessType_ACCESS_TYPE_LOAD_BALANCER, nil

}

func IsValidDeploymentManifestForResources(deploymentType, manifest string, kr *edgeproto.KubernetesResources) error {
	if deploymentType != DeploymentTypeKubernetes {
		return nil
	}
	objs, _, err := DecodeK8SYaml(manifest)
	if err != nil {
		return fmt.Errorf("parse kubernetes deployment yaml failed, %v", err)
	}
	return isValidKubernetesManifestForResources(objs, kr)
}

func isValidKubernetesManifestForResources(objs []runtime.Object, kr *edgeproto.KubernetesResources) error {
	count := KuberentesResourcesGPUCount(kr)
	if count == 0 {
		// currently we are only validating GPU resources
		return nil
	}
	var template *v1.PodTemplateSpec
	requestedGPUCount := int64(0)
	for i, _ := range objs {
		template = nil
		switch obj := objs[i].(type) {
		case *appsv1.Deployment:
			template = &obj.Spec.Template
		case *appsv1.DaemonSet:
			template = &obj.Spec.Template
		case *appsv1.StatefulSet:
			template = &obj.Spec.Template
		}
		if template == nil {
			continue
		}
		for j, _ := range template.Spec.Containers {
			resources := &template.Spec.Containers[j].Resources
			for _, gpuResName := range []v1.ResourceName{
				v1.ResourceName(KubernetesNvidiaGPUResource),
				v1.ResourceName(KubernetesAMDGPUResource),
			} {
				if qty, ok := resources.Limits[gpuResName]; ok {
					if val, valOk := qty.AsInt64(); valOk {
						requestedGPUCount += val
					}
				}
			}
		}
	}
	if requestedGPUCount > int64(count) {
		return fmt.Errorf("GPU resource limit (value:%v) exceeds flavor specified count %d", requestedGPUCount, count)
	}

	return nil
}

func IsValidDeploymentManifest(ctx context.Context, deploymentType, command, manifest string, ports []edgeproto.InstPort, kr *edgeproto.KubernetesResources) error {
	if deploymentType == DeploymentTypeVM {
		if command != "" {
			return fmt.Errorf("both deploymentmanifest and command cannot be used together for VM based deployment")
		}
		if strings.HasPrefix(manifest, "#cloud-config") {
			return nil
		}
		return fmt.Errorf("only cloud-init script support, must start with '#cloud-config'")
	} else if deploymentType == DeploymentTypeKubernetes {
		objs, _, err := DecodeK8SYaml(manifest)
		if err != nil {
			return fmt.Errorf("parse kubernetes deployment yaml failed, %v", err)
		}
		// check that any ports specified on App are part of manifest
		lbPorts := make(map[string]struct{})
		clusterPorts := make(map[string]struct{})
		for _, obj := range objs {
			ksvc, ok := obj.(*v1.Service)
			if !ok {
				continue
			}
			var portMap map[string]struct{}
			if ksvc.Spec.Type == v1.ServiceTypeLoadBalancer {
				portMap = lbPorts
			} else if ksvc.Spec.Type == v1.ServiceTypeClusterIP {
				portMap = clusterPorts
			} else {
				log.SpanLog(ctx, log.DebugLevelApi, "skipping unsupported service", "type", ksvc.Spec.Type)
				continue
			}
			for _, kp := range ksvc.Spec.Ports {
				appPort := dme.AppPort{}
				if kp.Protocol == "" {
					// default to TCP, as k8s does the same
					kp.Protocol = v1.ProtocolTCP
				}
				appPort.Proto, err = edgeproto.GetLProto(string(kp.Protocol))
				if err != nil {
					log.SpanLog(ctx, log.DebugLevelApi, "unrecognized port protocol in kubernetes manifest", "proto", string(kp.Protocol))
					continue
				}
				appPort.InternalPort = kp.Port
				portMap[appPort.String()] = struct{}{}
			}
		}
		missingPorts := []string{}
		for _, appPort := range ports {
			k8sProto := appPort.Proto
			if appPort.Proto == dme.LProto_L_PROTO_HTTP {
				// http ports are TCP ports in kubernetes services
				k8sProto = dme.LProto_L_PROTO_TCP
			}
			if appPort.EndPort != 0 {
				// We have a range-port notation on the dme.AppPort
				// while our manifest exhaustively enumerates each as a kubePort
				start := appPort.InternalPort
				end := appPort.EndPort
				// This is Kubernetes specific port range check, which is different only for UDP
				// Parseports() still checks for default range of ports (maxTcpPorts, maxUdpPorts, maxEnvoyUdpPorts)
				if appPort.Proto == dme.LProto_L_PROTO_UDP {
					portCount := end - start + 1
					if portCount > int32(util.MaxK8sUdpPorts) {
						return fmt.Errorf("Kubernetes deployment not allowed to specify more than %d udp ports", util.MaxK8sUdpPorts)
					}
				}
				for i := start; i <= end; i++ {
					// expand short hand notation to test membership in map
					tp := dme.AppPort{
						Proto:        k8sProto,
						InternalPort: int32(i),
						EndPort:      int32(0),
					}
					if _, found := lbPorts[tp.String()]; found {
						continue
					}
					if appPort.Proto == dme.LProto_L_PROTO_HTTP {
						// http ports may also be on cluster IPs,
						// as they will be routed via an ingress
						if _, found := clusterPorts[tp.String()]; found {
							continue
						}
					}
					protoStr, _ := edgeproto.LProtoStr(appPort.Proto)
					missingPorts = append(missingPorts, fmt.Sprintf("%s:%d", protoStr, tp.InternalPort))
				}
				continue
			}
			tp := edgeproto.InstPort{}
			tp.Proto = k8sProto
			tp.InternalPort = appPort.InternalPort
			if _, found := lbPorts[tp.String()]; found {
				continue
			}
			if appPort.Proto == dme.LProto_L_PROTO_HTTP {
				// http ports may also be on cluster IPs,
				// as they will be routed via an ingress
				if _, found := clusterPorts[tp.String()]; found {
					continue
				}
			}
			protoStr, _ := edgeproto.LProtoStr(tp.Proto)
			missingPorts = append(missingPorts, fmt.Sprintf("%s:%d", protoStr, tp.InternalPort))
		}
		if len(missingPorts) > 0 {
			log.SpanLog(ctx, log.DebugLevelApi, "validate deployment manifest failed", "missing ports", missingPorts, "lbPorts", fmt.Sprintf("%v", lbPorts), "clusterPorts", fmt.Sprintf("%v", clusterPorts))
			return fmt.Errorf("port %s defined in AccessPorts but missing from kubernetes manifest in a LoadBalancer service (TCP/UDP/HTTP) or ClusterIP service (HTTP only)", strings.Join(missingPorts, ","))
		}
		err = isValidKubernetesManifestForResources(objs, kr)
		if err != nil {
			return err
		}
	}
	return nil
}

func GetDefaultDeploymentType(imageType edgeproto.ImageType) (string, error) {
	switch imageType {
	case edgeproto.ImageType_IMAGE_TYPE_DOCKER:
		return DeploymentTypeKubernetes, nil
	case edgeproto.ImageType_IMAGE_TYPE_QCOW:
		fallthrough
	case edgeproto.ImageType_IMAGE_TYPE_OVF:
		fallthrough
	case edgeproto.ImageType_IMAGE_TYPE_OVA:
		return DeploymentTypeVM, nil
	case edgeproto.ImageType_IMAGE_TYPE_HELM:
		return DeploymentTypeHelm, nil
	}
	return "", fmt.Errorf("unknown image type %s", imageType)
}

func GetImageTypeForDeployment(deployment string) (edgeproto.ImageType, error) {
	switch deployment {
	case DeploymentTypeDocker:
		fallthrough
	case DeploymentTypeKubernetes:
		return edgeproto.ImageType_IMAGE_TYPE_DOCKER, nil
	case DeploymentTypeHelm:
		return edgeproto.ImageType_IMAGE_TYPE_HELM, nil
	case DeploymentTypeVM:
		// could be different formats
		fallthrough
	default:
		return edgeproto.ImageType_IMAGE_TYPE_UNKNOWN, nil
	}
}

// GetAppDeploymentManifest gets the deployment-specific manifest.
func GetAppDeploymentManifest(ctx context.Context, authApi RegistryAuthApi, app *edgeproto.App) (string, error) {
	if app.DeploymentManifest != "" {
		return GetDeploymentManifest(ctx, authApi, app.DeploymentManifest)
	} else if app.DeploymentGenerator != "" {
		return GenerateManifest(app)
	} else if app.Deployment == DeploymentTypeKubernetes {
		// kubernetes requires a deployment yaml. Use default generator.
		app.DeploymentGenerator = deploygen.KubernetesBasic
		str, err := GenerateManifest(app)
		if err != nil {
			return "", fmt.Errorf("failed to use default deployment generator %s, %s", app.Deployment, err.Error())
		}
		return str, nil
	}
	// no manifest specified
	return "", nil
}

func GetRemoteZipDockerManifests(ctx context.Context, authApi RegistryAuthApi, manifest, zipfile, downloadAction string) ([]map[string]DockerContainer, error) {
	if zipfile == "" {
		zipfile = "/var/tmp/temp.zip"
	}
	if downloadAction == Download {
		err := GetRemoteManifestToFile(ctx, authApi, manifest, zipfile)
		if err != nil {
			return nil, fmt.Errorf("cannot get manifest from %s, %v", manifest, err)
		}
	}
	defer os.Remove(zipfile)
	r, err := zip.OpenReader(zipfile)
	if err != nil {
		return nil, fmt.Errorf("cannot read zipfile from manifest %s, %v", manifest, err)
	}
	defer r.Close()
	foundManifest := false
	var filesInManifest = make(map[string]*zip.File)
	var dm DockerManifest
	for _, f := range r.File {
		filesInManifest[f.Name] = f
		if f.Name == "manifest.yml" {
			foundManifest = true
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("cannot open manifest.yml in zipfile: %v", err)
			}
			buf := new(bytes.Buffer)
			buf.ReadFrom(rc)
			rc.Close()
			err = yaml.Unmarshal(buf.Bytes(), &dm)
			if err != nil {
				return nil, fmt.Errorf("unmarshalling manifest.yml: %v", err)
			}
		}
	}
	if !foundManifest {
		return nil, fmt.Errorf("no manifest.yml in zipfile %s", manifest)
	}
	var zipContainers []map[string]DockerContainer
	for _, dc := range dm.DockerComposeFiles {
		f, ok := filesInManifest[dc]
		if !ok {
			return nil, fmt.Errorf("docker-compose file specified in manifest but not in zip: %s", dc)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("cannot open docker compose file %s in zipfile: %v", dc, err)
		}
		buf := new(bytes.Buffer)
		buf.ReadFrom(rc)
		rc.Close()

		content := buf.String()
		containers, err := DecodeDockerComposeYaml(content)
		if err != nil {
			return nil, fmt.Errorf("failed to parse %s manifest file with contents %s: %v", dc, content, err)
		}
		zipContainers = append(zipContainers, containers)
	}
	return zipContainers, nil
}

func validateRemoteZipManifest(ctx context.Context, authApi RegistryAuthApi, manifest string) error {
	_, err := GetRemoteZipDockerManifests(ctx, authApi, manifest, "", Download)
	return err
}

func GetDeploymentManifest(ctx context.Context, authApi RegistryAuthApi, manifest string) (string, error) {
	// manifest may be remote target or inline json/yaml
	if strings.HasPrefix(manifest, "http://") || strings.HasPrefix(manifest, "https://") {

		if strings.HasSuffix(manifest, ".zip") {
			log.SpanLog(ctx, log.DebugLevelApi, "zipfile manifest found", "manifest", manifest)
			return manifest, validateRemoteZipManifest(ctx, authApi, manifest)
		}
		mf, err := GetRemoteManifest(ctx, authApi, manifest)
		if err != nil {
			return "", fmt.Errorf("cannot get manifest from %s, %v", manifest, err)
		}
		return mf, nil
	}
	// inline manifest
	return manifest, nil
}

func GenerateManifest(app *edgeproto.App) (string, error) {
	target := app.DeploymentGenerator
	if target == "" {
		return "", fmt.Errorf("no deployment generator specified")
	}
	// generator may be remote target or generator name
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		return deploygen.SendReq(target, app)
	} else if _, ok := deploygen.Generators[target]; ok {
		return deploygen.RunGen(target, app)
	}
	return "", fmt.Errorf("invalid deployment generator %s", target)
}

func GetRemoteManifest(ctx context.Context, authApi RegistryAuthApi, target string) (string, error) {
	var content string
	err := DownloadFile(ctx, authApi, target, NoCreds, "", &content)
	if err != nil {
		return "", err
	}
	return content, nil
}

func GetRemoteManifestToFile(ctx context.Context, authApi RegistryAuthApi, target string, filename string) error {
	return DownloadFile(ctx, authApi, target, NoCreds, filename, nil)
}

// 5GB = 10minutes
func GetTimeout(cLen int) time.Duration {
	fileSizeInGB := float64(cLen) / (1024.0 * 1024.0 * 1024.0)
	timeoutUnit := int(math.Ceil(fileSizeInGB / 5.0))
	if fileSizeInGB > 5 {
		return time.Duration(timeoutUnit) * 10 * time.Minute
	}
	return 15 * time.Minute
}

func DownloadFile(ctx context.Context, authApi RegistryAuthApi, fileUrlPath, urlCreds, filePath string, content *string) (reterr error) {
	var reqConfig *RequestConfig

	log.SpanLog(ctx, log.DebugLevelApi, "attempt to download file", "file-url", fileUrlPath)

	// Adjust request timeout based on File Size
	//  - Timeout is increased by 10min for every 5GB
	//  - If less than 5GB, then use default timeout
	resp, err := SendHTTPReq(ctx, "HEAD", fileUrlPath, authApi, urlCreds, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	contentLength := resp.Header.Get("Content-Length")
	cLen, err := strconv.Atoi(contentLength)
	if err == nil && cLen > 0 {
		timeout := GetTimeout(cLen)
		if timeout > 0 {
			reqConfig = &RequestConfig{
				Timeout: timeout,
			}
			log.SpanLog(ctx, log.DebugLevelApi, "increased request timeout", "file-url", fileUrlPath, "timeout", timeout.String())
		}
	}

	resp, err = SendHTTPReq(ctx, "GET", fileUrlPath, authApi, urlCreds, reqConfig, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Check server response
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status: %s", resp.Status)
	}

	if filePath != "" {
		// Create the file
		out, err := os.Create(filePath)
		if err != nil {
			return err
		}
		defer func() {
			out.Close()
			if reterr != nil {
				// Stale file might be present if download fails/succeeds, deleting it
				if delerr := DeleteFile(filePath); delerr != nil {
					log.SpanLog(ctx, log.DebugLevelApi, "file cleanup failed", "filePath", filePath)
				}
			}
		}()
		_, err = io.Copy(out, resp.Body)
		if err != nil {
			return fmt.Errorf("failed to download file %v", err)
		}
	}

	if content != nil {
		contentBytes, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		*content = string(contentBytes)
	}

	return nil
}

// Transform AppInst deployment type to ClusterInst deployment type
func AppInstToClusterDeployment(deployment string) string {
	if deployment == DeploymentTypeHelm {
		deployment = DeploymentTypeKubernetes
	}
	return deployment
}

func AppDeploysToKubernetes(deployment string) bool {
	return deployment == DeploymentTypeKubernetes || deployment == DeploymentTypeHelm
}

// GetAppInstOwner gets the org "owner". For local appInsts, the
// owner is the Key.Organization. For federated AppInsts, the owner
// is the AppProviderId (Key.Organization is set to the fedhost).
// This assumes AppProviderId is globally unique and comparable
// across all platforms, which is probably not true. It should be
// reworked to be able to map a partner fed + remote AppProviderId
// to a local Organization.
func GetAppInstOwner(appInst *edgeproto.AppInst) edgeproto.OrgName {
	if partnerAppProviderID, found := appInst.Annotations[AnnotationFedPartnerAppProviderID]; found {
		return edgeproto.OrgName(partnerAppProviderID)
	}
	return edgeproto.OrgName(appInst.Key.Organization)
}
