// Package deploy renders ECS JSON only. Nothing here calls AWS; `stormo deploy render` prints the
// aws commands to apply it, so registering and rolling a service stays an explicit human step.
//
// Task shape (one task per agent, desiredCount 1: a Hermes gateway is a single writer per home):
//
//	rehydrate  sidecar image, runs once, essential=false        → fills the shared volume
//	nap        sidecar image, long-running, dependsOn rehydrate  → snapshots to S3, final nap on SIGTERM
//	agent      engine image, dependsOn rehydrate SUCCESS + nap START
//
// ECS stops containers in reverse dependency order, so on shutdown the agent stops (and flushes)
// before nap receives SIGTERM and takes the last snapshot of a quiesced home.
package deploy

import (
	"fmt"
	"strconv"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/env"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/shared"
)

// Target is the instance's AWS target plus the sidecar image tag being deployed.
type Target struct {
	instance.AwsTarget
	// Resource is the instance's names.resource (ECS family/service, log group, image prefix).
	Resource   string
	SidecarTag string
}

// DefaultTarget is the instance's AWS target (stormo.yaml deploy.aws) with sidecar tag "latest".
func DefaultTarget(inst *instance.Instance) Target {
	aws := inst.Aws
	aps := map[string]string{}
	for k, v := range aws.Efs.AccessPoints {
		aps[k] = v
	}
	aws.Efs.AccessPoints = aps
	return Target{AwsTarget: aws, Resource: inst.Names.Resource, SidecarTag: "latest"}
}

// AccessPointID is the EFS access point of a layer, or <unset>.
func AccessPointID(t Target, layer string) string {
	if v, ok := t.Efs.AccessPoints[layer]; ok {
		return v
	}
	return instance.Unset
}

const volume = "home"

// SidecarImage is the per-agent sidecar image reference.
func SidecarImage(a *manifest.Agent, t Target) string {
	registry := t.Registry
	if a.Deploy.Registry == "ecr" {
		registry = fmt.Sprintf("%s.dkr.ecr.%s.amazonaws.com", t.Account, t.Region)
	}
	return fmt.Sprintf("%s/%s-%s:%s", registry, t.Resource, a.ID, t.SidecarTag)
}

// JSON shapes; field order is the rendered file's order, keep it stable.

type LogOptions struct {
	Group        string `json:"awslogs-group"`
	Region       string `json:"awslogs-region"`
	StreamPrefix string `json:"awslogs-stream-prefix"`
	CreateGroup  string `json:"awslogs-create-group"`
}

type LogConfiguration struct {
	LogDriver string     `json:"logDriver"`
	Options   LogOptions `json:"options"`
}

func logs(a *manifest.Agent, t Target, stream string) LogConfiguration {
	return LogConfiguration{"awslogs", LogOptions{
		Group: fmt.Sprintf("/ecs/%s-%s", t.Resource, a.ID), Region: t.Region, StreamPrefix: stream, CreateGroup: "true",
	}}
}

type RuntimePlatform struct {
	CPUArchitecture       string `json:"cpuArchitecture"`
	OperatingSystemFamily string `json:"operatingSystemFamily"`
}

type EphemeralStorage struct {
	SizeInGiB int `json:"sizeInGiB"`
}

type AuthorizationConfig struct {
	AccessPointID string `json:"accessPointId"`
	IAM           string `json:"iam"`
}

type EfsVolumeConfiguration struct {
	FileSystemID        string              `json:"fileSystemId"`
	TransitEncryption   string              `json:"transitEncryption"`
	AuthorizationConfig AuthorizationConfig `json:"authorizationConfig"`
}

type Volume struct {
	Name                   string                  `json:"name"`
	EfsVolumeConfiguration *EfsVolumeConfiguration `json:"efsVolumeConfiguration,omitempty"`
}

type MountPoint struct {
	SourceVolume  string `json:"sourceVolume"`
	ContainerPath string `json:"containerPath"`
}

type DependsOn struct {
	ContainerName string `json:"containerName"`
	Condition     string `json:"condition"`
}

type Secret struct {
	Name      string `json:"name"`
	ValueFrom string `json:"valueFrom"`
}

type PortMapping struct {
	ContainerPort int    `json:"containerPort"`
	Protocol      string `json:"protocol"`
}

type HealthCheck struct {
	Command     []string `json:"command"`
	Interval    int      `json:"interval"`
	Timeout     int      `json:"timeout"`
	Retries     int      `json:"retries"`
	StartPeriod int      `json:"startPeriod"`
}

type Container struct {
	Name             string           `json:"name"`
	Image            string           `json:"image"`
	Essential        bool             `json:"essential"`
	Command          []string         `json:"command"`
	Environment      []env.NameValue  `json:"environment"`
	Secrets          *[]Secret        `json:"secrets,omitempty"`
	MountPoints      []MountPoint     `json:"mountPoints"`
	PortMappings     []PortMapping    `json:"portMappings,omitempty"`
	HealthCheck      *HealthCheck     `json:"healthCheck,omitempty"`
	DependsOn        []DependsOn      `json:"dependsOn,omitempty"`
	StopTimeout      int              `json:"stopTimeout,omitempty"`
	LogConfiguration LogConfiguration `json:"logConfiguration"`
}

type TaskDef struct {
	Family                  string            `json:"family"`
	NetworkMode             string            `json:"networkMode"`
	RequiresCompatibilities []string          `json:"requiresCompatibilities"`
	RuntimePlatform         RuntimePlatform   `json:"runtimePlatform"`
	CPU                     string            `json:"cpu"`
	Memory                  string            `json:"memory"`
	EphemeralStorage        *EphemeralStorage `json:"ephemeralStorage,omitempty"`
	ExecutionRoleArn        string            `json:"executionRoleArn"`
	TaskRoleArn             string            `json:"taskRoleArn"`
	// Task-local scratch volume. Not EFS: Hermes' SQLite runs in WAL mode, which NFS corrupts. If
	// this ever moves to EFS, set database.journal_mode: delete in the engine overrides first.
	Volumes              []Volume    `json:"volumes"`
	ContainerDefinitions []Container `json:"containerDefinitions"`
}

// RenderTaskDef is the agent's ECS task definition.
func RenderTaskDef(a *manifest.Agent, eng engine.Engine, t Target) TaskDef {
	secretArn := fmt.Sprintf("arn:aws:secretsmanager:%s:%s:secret:%s", t.Region, t.Account, a.Deploy.SecretID)
	sidecar := SidecarImage(a, t)
	swarmEnv := []env.NameValue{
		{Name: "SWARM_AGENT", Value: a.ID},
		{Name: "SWARM_STORE", Value: "s3://" + t.Bucket},
		{Name: "SWARM_HOME", Value: "/data"},
		{Name: "AWS_REGION", Value: t.Region},
	}
	fargate := a.Deploy.Launch == "fargate"
	compat := "EC2"
	var eph *EphemeralStorage
	if fargate {
		compat = "FARGATE"
		eph = &EphemeralStorage{a.Deploy.EphemeralStorageGiB}
	}
	layers := shared.Layers(a)
	volumes := []Volume{{Name: volume}}
	// Shared documents: EFS through a per-layer access point (root /<layer>, uid 10000),
	// IAM-authorised, so a task can only mount the access points its role names. Only the agent
	// container mounts them.
	for _, l := range layers {
		volumes = append(volumes, Volume{Name: "shared-" + l.Layer, EfsVolumeConfiguration: &EfsVolumeConfiguration{
			FileSystemID: t.Efs.FileSystemID, TransitEncryption: "ENABLED",
			AuthorizationConfig: AuthorizationConfig{AccessPointID: AccessPointID(t, l.Layer), IAM: "ENABLED"},
		}})
	}
	napEnv := append(append([]env.NameValue{}, swarmEnv...), env.NameValue{Name: "SWARM_NAP_INTERVAL", Value: strconv.Itoa(a.Learning.NapIntervalSeconds)})
	// Each secret is a JSON key in one Secrets Manager secret per agent; the sidecars get none.
	secrets := make([]Secret, 0, len(a.Secrets))
	for _, n := range a.Secrets {
		secrets = append(secrets, Secret{n, fmt.Sprintf("%s:%s::", secretArn, n)})
	}
	agentMounts := []MountPoint{{volume, eng.Layout().Home}}
	for _, l := range layers {
		agentMounts = append(agentMounts, MountPoint{"shared-" + l.Layer, l.Path})
	}
	agentEnv := eng.Env(a, manifest.AWS).Pairs()
	return TaskDef{
		Family:                  t.Resource + "-" + a.ID,
		NetworkMode:             "awsvpc",
		RequiresCompatibilities: []string{compat},
		RuntimePlatform:         RuntimePlatform{"ARM64", "LINUX"},
		CPU:                     strconv.Itoa(a.Deploy.CPU),
		Memory:                  strconv.Itoa(a.Deploy.Memory),
		EphemeralStorage:        eph,
		ExecutionRoleArn:        t.ExecutionRoleArn,
		TaskRoleArn:             t.TaskRoleArn,
		Volumes:                 volumes,
		ContainerDefinitions: []Container{
			{
				Name: "rehydrate", Image: sidecar, Essential: false, Command: []string{"rehydrate"},
				Environment: swarmEnv, MountPoints: []MountPoint{{volume, "/data"}},
				LogConfiguration: logs(a, t, "rehydrate"),
			},
			{
				Name: "nap", Image: sidecar, Essential: false, Command: []string{"nap", "--loop"},
				Environment: napEnv, MountPoints: []MountPoint{{volume, "/data"}},
				DependsOn:   []DependsOn{{"rehydrate", "SUCCESS"}},
				StopTimeout: 120, LogConfiguration: logs(a, t, "nap"),
			},
			{
				Name: "agent", Image: eng.Image(a), Essential: true, Command: eng.Command(a),
				Environment: agentEnv, Secrets: &secrets, MountPoints: agentMounts,
				PortMappings: []PortMapping{{eng.Port(), "tcp"}},
				HealthCheck:  &HealthCheck{Command: eng.HealthCheck(), Interval: 30, Timeout: 5, Retries: 3, StartPeriod: 120},
				DependsOn:    []DependsOn{{"rehydrate", "SUCCESS"}, {"nap", "START"}},
				StopTimeout:  60, LogConfiguration: logs(a, t, "agent"),
			},
		},
	}
}

type PolicyStatement struct {
	Effect    string         `json:"Effect"`
	Action    []string       `json:"Action"`
	Resource  []string       `json:"Resource"`
	Condition map[string]any `json:"Condition,omitempty"`
}

type Policy struct {
	Version   string            `json:"Version"`
	Statement []PolicyStatement `json:"Statement"`
}

// RenderTaskPolicy is the least-privilege task role policy: this agent's prefix only. Dream runs
// with a separate role that is denied `<agent>/raw/*`.
func RenderTaskPolicy(a *manifest.Agent, t Target) Policy {
	aps := []string{}
	for _, l := range shared.Layers(a) {
		aps = append(aps, fmt.Sprintf("arn:aws:elasticfilesystem:%s:%s:access-point/%s", t.Region, t.Account, AccessPointID(t, l.Layer)))
	}
	return Policy{
		Version: "2012-10-17",
		Statement: []PolicyStatement{
			{Effect: "Allow", Action: []string{"s3:GetObject", "s3:PutObject"}, Resource: []string{fmt.Sprintf("arn:aws:s3:::%s/%s/*", t.Bucket, a.ID)}},
			{Effect: "Allow", Action: []string{"s3:ListBucket"}, Resource: []string{"arn:aws:s3:::" + t.Bucket},
				Condition: map[string]any{"StringLike": map[string]any{"s3:prefix": []string{a.ID + "/*"}}}},
			{Effect: "Allow", Action: []string{"elasticfilesystem:ClientMount", "elasticfilesystem:ClientWrite"},
				Resource:  []string{fmt.Sprintf("arn:aws:elasticfilesystem:%s:%s:file-system/%s", t.Region, t.Account, t.Efs.FileSystemID)},
				Condition: map[string]any{"StringEquals": map[string]any{"elasticfilesystem:AccessPointArn": aps}}},
		},
	}
}

// Unresolved lists placeholders that make a rendered task definition undeployable.
func Unresolved(a *manifest.Agent, t Target) []string {
	out := []string{}
	if t.Efs.FileSystemID == instance.Unset {
		out = append(out, "efs.fileSystemId")
	}
	for _, l := range shared.Layers(a) {
		if AccessPointID(t, l.Layer) == instance.Unset {
			out = append(out, "efs.accessPoints."+l.Layer)
		}
	}
	return out
}

// SharedAccessPointCommands is the one-time setup of the shared space: an access point per layer,
// owned by the engine's runtime uid.
func SharedAccessPointCommands(layers []string, t Target) []string {
	fs := t.Efs.FileSystemID
	if fs == instance.Unset {
		fs = "<fs-id>"
	}
	uid := shared.PosixUID
	out := []string{
		fmt.Sprintf("# 1. once: aws efs create-file-system --region %s --encrypted --performance-mode generalPurpose --throughput-mode elastic --tags Key=Name,Value=%s-shared", t.Region, t.Resource),
		"#    plus a mount target per private subnet with an SG allowing NFS (2049) from the agent tasks",
		"# 2. one access point per layer:",
	}
	for _, layer := range layers {
		out = append(out, fmt.Sprintf("aws efs create-access-point --region %s --file-system-id %s ", t.Region, fs)+
			fmt.Sprintf("--posix-user Uid=%d,Gid=%d ", uid, uid)+
			fmt.Sprintf("--root-directory 'Path=/%s,CreationInfo={OwnerUid=%d,OwnerGid=%d,Permissions=2775}' ", layer, uid, uid)+
			fmt.Sprintf("--tags Key=Name,Value=%s-shared-%s", t.Resource, layer))
	}
	return append(out, "# 3. record the fs id and access point ids in deploy.aws.efs (stormo.yaml)")
}

// ApplyCommands are the aws commands that register and roll the rendered task definition; dir is
// where taskdef.json was written ("dist/<agent>" by default).
func ApplyCommands(a *manifest.Agent, t Target, dir string) []string {
	if dir == "" {
		dir = "dist/" + a.ID
	}
	svc := t.Resource + "-" + a.ID
	launch := ""
	if a.Deploy.Launch == "fargate" {
		launch = " --launch-type FARGATE --network-configuration 'awsvpcConfiguration={subnets=[<subnet>],securityGroups=[<sg>],assignPublicIp=ENABLED}'"
	}
	return []string{
		fmt.Sprintf("aws ecs register-task-definition --region %s --cli-input-json file://%s/taskdef.json", t.Region, dir),
		`# first time only (subnets/SG are not in the manifest yet, see ARCHITECTURE.md "Open decisions"):`,
		fmt.Sprintf("aws ecs create-service --region %s --cluster %s --service-name %s --task-definition %s --desired-count 1 --deployment-configuration maximumPercent=100,minimumHealthyPercent=0%s", t.Region, t.Cluster, svc, svc, launch),
		fmt.Sprintf("aws ecs update-service --region %s --cluster %s --service %s --task-definition %s", t.Region, t.Cluster, svc, svc),
	}
}
