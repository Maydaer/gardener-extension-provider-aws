// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package bastion

import (
	"context"
	"fmt"
	"regexp"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/gardener/gardener/extensions/pkg/controller"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"github.com/gardener/gardener/pkg/client/kubernetes"
	"github.com/gardener/gardener/pkg/extensions"
	"k8s.io/apimachinery/pkg/util/sets"

	awsv1alpha1 "github.com/gardener/gardener-extension-provider-aws/pkg/apis/aws/v1alpha1"
	awsclient "github.com/gardener/gardener-extension-provider-aws/pkg/aws/client"
)

// Options contains provider-related information required for setting up
// a bastion instance. This struct combines precomputed values like the
// bastion instance name with the IDs of pre-existing cloud provider
// resources, like the VPC ID, subnet ID etc.
type Options struct {
	Shoot                    *gardencorev1beta1.Shoot
	SubnetID                 string
	VPCID                    string
	BastionSecurityGroupName string
	WorkerSecurityGroupName  string
	WorkerSecurityGroupID    string
	InstanceName             string
	InstanceType             string
	ImageID                  string

	// set later during reconciling phase
	BastionSecurityGroupID string
}

// DetermineOptions determines the required information like VPC ID and
// instance type that are required to reconcile a Bastion on AWS. This
// function does not create any IaaS resources.
func DetermineOptions(ctx context.Context, bastion *extensionsv1alpha1.Bastion, cluster *controller.Cluster, awsClient *awsclient.Client) (*Options, error) {
	name := cluster.ObjectMeta.Name
	subnetName := name + "-public-utility-z0"
	instanceName := fmt.Sprintf("%s-%s-bastion", name, bastion.Name)

	// this security group will be created during reconciliation
	bastionSecurityGroupName := fmt.Sprintf("%s-%s-bsg", name, bastion.Name)

	subnetID, vpcID, err := resolveSubnetName(ctx, awsClient, subnetName)
	if err != nil {
		return nil, fmt.Errorf("failed to find subnet %q: %w", subnetName, err)
	}

	// this security group exists already and just needs to be resolved to its ID
	workerSecurityGroupName := name + "-nodes"
	workerSecurityGroup, err := getSecurityGroup(ctx, awsClient, vpcID, workerSecurityGroupName)
	if err != nil {
		return nil, fmt.Errorf("failed to check for worker security group: %w", err)
	}
	if workerSecurityGroup == nil {
		return nil, fmt.Errorf("security group for worker node does not exist yet")
	}

	cloudProfileConfig, err := getCloudProfileConfig(cluster)
	if err != nil {
		return nil, fmt.Errorf("failed to extract cloud provider config from cluster: %w", err)
	}

	imageID, err := determineImageID(cluster.Shoot, cloudProfileConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to determine OS image for bastion host: %w", err)
	}

	instanceType, err := determineInstanceType(ctx, imageID, awsClient)
	if err != nil {
		return nil, fmt.Errorf("failed to determine instance type: %w", err)
	}

	return &Options{
		Shoot:                    cluster.Shoot,
		SubnetID:                 subnetID,
		VPCID:                    vpcID,
		BastionSecurityGroupName: bastionSecurityGroupName,
		WorkerSecurityGroupName:  workerSecurityGroupName,
		WorkerSecurityGroupID:    *workerSecurityGroup.GroupId,
		InstanceName:             instanceName,
		InstanceType:             instanceType,
		ImageID:                  imageID,
	}, nil
}

// resolveSubnetName resolves a subnet name to its ID and the VPC ID. If no subnet with the
// given name exists, an error is returned.
func resolveSubnetName(ctx context.Context, awsClient *awsclient.Client, subnetName string) (subnetID string, vpcID string, err error) {
	subnets, err := awsClient.EC2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("tag:Name"),
				Values: []string{subnetName},
			},
		},
	})
	if err != nil {
		return
	}

	if len(subnets.Subnets) == 0 {
		err = fmt.Errorf("subnet not found")
		return
	}

	subnetID = *subnets.Subnets[0].SubnetId
	vpcID = *subnets.Subnets[0].VpcId

	return
}

func getCloudProfileConfig(cluster *extensions.Cluster) (*awsv1alpha1.CloudProfileConfig, error) {
	if cluster.CloudProfile.Spec.ProviderConfig.Raw == nil {
		return nil, fmt.Errorf("no cloud provider config set in cluster's CloudProfile")
	}

	var (
		cloudProfileConfig = &awsv1alpha1.CloudProfileConfig{}
		decoder            = kubernetes.GardenCodec.UniversalDeserializer()
	)

	if _, _, err := decoder.Decode(cluster.CloudProfile.Spec.ProviderConfig.Raw, nil, cloudProfileConfig); err != nil {
		return nil, err
	}

	return cloudProfileConfig, nil
}

// determineImageID finds the first AMI that is configured for the same region as the shoot cluster.
// If no image is found, an error is returned.
func determineImageID(shoot *gardencorev1beta1.Shoot, providerConfig *awsv1alpha1.CloudProfileConfig) (string, error) {
	// TODO(hebelsan): remove version hack after bastion image is well defined, e.g. in cloudProfile
	// only allow garden linux versions 1312.x.x because they have ssh enabled by default
	re := regexp.MustCompile(`^1312\.\d+\.\d+$`)
	for _, image := range providerConfig.MachineImages {
		for _, version := range image.Versions {
			if image.Name == "gardenlinux" && !re.MatchString(version.Version) {
				continue
			}
			for _, region := range version.Regions {
				if region.Name == shoot.Spec.Region {
					return region.AMI, nil
				}
			}
		}
	}

	return "", fmt.Errorf("found no suitable AMI for machines in region %q", shoot.Spec.Region)
}

func determineInstanceType(ctx context.Context, imageID string, awsClient *awsclient.Client) (string, error) {
	var preferredType string
	imageInfo, err := getImages(ctx, imageID, awsClient)
	if err != nil {
		return "", err
	}

	imageArchitecture := imageInfo.Architecture

	// default instance type
	switch imageArchitecture {
	case ec2types.ArchitectureValuesX8664:
		preferredType = "t2.nano"
	case ec2types.ArchitectureValuesArm64:
		preferredType = "t4g.nano"
	default:
		return "", fmt.Errorf("image architecture not supported")
	}

	exist, err := getInstanceTypeOfferings(ctx, preferredType, awsClient)
	if err != nil {
		return "", err
	}

	if len(exist.InstanceTypeOfferings) != 0 {
		return preferredType, nil
	}

	// filter t type instance
	tTypes, err := getInstanceTypeOfferings(ctx, "t*", awsClient)
	if err != nil {
		return "", err
	}

	if len(tTypes.InstanceTypeOfferings) == 0 {
		return "", fmt.Errorf("no t* instance type offerings available")
	}

	tTypeSet := sets.Set[ec2types.InstanceType]{}
	for _, t := range tTypes.InstanceTypeOfferings {
		tTypeSet.Insert(t.InstanceType)
	}

	result, err := awsClient.EC2.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{
		InstanceTypes: tTypeSet.UnsortedList(),
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("processor-info.supported-architecture"),
				Values: []string{string(imageArchitecture)},
			},
		},
	})

	if err != nil {
		return "", err
	}

	if len(result.InstanceTypes) == 0 {
		return "", fmt.Errorf("no instance types returned for architecture %s and instance types list %v", imageArchitecture, tTypeSet.UnsortedList())
	}

	return string(result.InstanceTypes[0].InstanceType), nil
}

func getImages(ctx context.Context, ami string, awsClient *awsclient.Client) (*ec2types.Image, error) {
	imageInfo, err := awsClient.EC2.DescribeImages(ctx, &ec2.DescribeImagesInput{
		ImageIds: []string{ami},
	})

	if err != nil {
		return nil, fmt.Errorf("failed to get Images Info: %w", err)
	}

	if len(imageInfo.Images) == 0 {
		return nil, fmt.Errorf("images info not found: %w", err)
	}
	return &imageInfo.Images[0], nil
}

func getInstanceTypeOfferings(ctx context.Context, filter string, awsClient *awsclient.Client) (*ec2.DescribeInstanceTypeOfferingsOutput, error) {
	return awsClient.EC2.DescribeInstanceTypeOfferings(ctx, &ec2.DescribeInstanceTypeOfferingsInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("instance-type"),
				Values: []string{filter},
			},
		},
	})
}
