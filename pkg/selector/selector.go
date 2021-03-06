// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

// Package selector provides filtering logic for Amazon EC2 Instance Types based on declarative resource specfications.
package selector

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/aws/amazon-ec2-instance-selector/pkg/selector/outputs"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
)

var (
	// Version is overridden at compilation with the version based on the git tag
	versionID = "dev"
)

const (
	// regionNameRegex Matches strings like: us-east-1 or us-east-2
	regionNameRegex = `^[a-z]{2,3}\-([a-z]{1,10}\-)?[a-z]{1,10}\-[1-9]`
	// zoneIDRegex Matches strings like: use1-az1 or use2-az3
	zoneIDRegex = `^[a-z]{3}[1-9]{1}\-az[1-9]$`
	// zoneNameRegex Matches strings like: us-east-1a or us-east-2c
	zoneNameRegex          = `^[a-z]{2,3}\-([a-z]{1,10}\-)?[a-z]{1,10}\-[1-9][a-z]$`
	locationFilterKey      = "location"
	zoneIDLocationType     = "availability-zone-id"
	zoneNameLocationType   = "availability-zone"
	regionNameLocationType = "region"
	sdkName                = "instance-selector"

	// Filter Keys

	cpuArchitecture        = "cpuArchitecture"
	usageClass             = "usageClass"
	rootDeviceType         = "rootDeviceType"
	hibernationSupported   = "hibernationSupported"
	vcpusRange             = "vcpusRange"
	memoryRange            = "memoryRange"
	gpuMemoryRange         = "gpuMemoryRange"
	gpusRange              = "gpusRange"
	placementGroupStrategy = "placementGroupStrategy"
	hypervisor             = "hypervisor"
	baremetal              = "baremetal"
	burstable              = "burstable"
	fpga                   = "fpga"
	enaSupport             = "enaSupport"
	vcpusToMemoryRatio     = "vcpusToMemoryRatio"
	currentGeneration      = "currentGeneration"
	networkInterfaces      = "networkInterfaces"
	networkPerformance     = "networkPerformance"
)

// New creates an instance of Selector provided an aws session
func New(sess *session.Session) *Selector {
	userAgentTag := fmt.Sprintf("%s-v%s", sdkName, versionID)
	userAgentHandler := request.MakeAddToUserAgentFreeFormHandler(userAgentTag)
	sess.Handlers.Build.PushBack(userAgentHandler)
	return &Selector{
		EC2: ec2.New(sess),
	}
}

// Filter accepts a Filters struct which is used to select the available instance types
// matching the criteria within Filters and returns a simple list of instance type strings
func (itf Selector) Filter(filters Filters) ([]string, error) {
	outputFn := InstanceTypesOutputFn(outputs.SimpleInstanceTypeOutput)
	return itf.FilterWithOutput(filters, outputFn)
}

// FilterVerbose accepts a Filters struct which is used to select the available instance types
// matching the criteria within Filters and returns a list instanceTypeInfo
func (itf Selector) FilterVerbose(filters Filters) ([]*ec2.InstanceTypeInfo, error) {
	instanceTypeInfoSlice, err := itf.rawFilter(filters)
	if err != nil {
		return nil, err
	}
	instanceTypeInfoSlice = itf.truncateResults(filters.MaxResults, instanceTypeInfoSlice)
	return instanceTypeInfoSlice, nil
}

// FilterWithOutput accepts a Filters struct which is used to select the available instance types
// matching the criteria within Filters and returns a list of strings based on the custom outputFn
func (itf Selector) FilterWithOutput(filters Filters, outputFn InstanceTypesOutput) ([]string, error) {
	instanceTypeInfoSlice, err := itf.rawFilter(filters)
	if err != nil {
		return nil, err
	}
	instanceTypeInfoSlice = itf.truncateResults(filters.MaxResults, instanceTypeInfoSlice)
	output := outputFn.Output(instanceTypeInfoSlice)
	return output, nil
}

func (itf Selector) truncateResults(maxResults *int, instanceTypeInfoSlice []*ec2.InstanceTypeInfo) []*ec2.InstanceTypeInfo {
	if maxResults == nil {
		return instanceTypeInfoSlice
	}
	upperIndex := *maxResults
	if *maxResults > len(instanceTypeInfoSlice) {
		upperIndex = len(instanceTypeInfoSlice)
	}
	return instanceTypeInfoSlice[0:upperIndex]
}

// rawFilter accepts a Filters struct which is used to select the available instance types
// matching the criteria within Filters and returns the detailed specs of matching instance types
func (itf Selector) rawFilter(filters Filters) ([]*ec2.InstanceTypeInfo, error) {
	var location string
	if filters.AvailabilityZone != nil {
		location = *filters.AvailabilityZone
	} else if filters.Region != nil {
		location = *filters.Region
	}
	locationInstanceOfferings, err := itf.RetrieveInstanceTypesSupportedInLocation(location)
	if err != nil {
		return nil, err
	}

	instanceTypesInput := &ec2.DescribeInstanceTypesInput{}
	instanceTypeCandidates := map[string]*ec2.InstanceTypeInfo{}
	// innerErr will hold any error while processing DescribeInstanceTypes pages
	var innerErr error

	err = itf.EC2.DescribeInstanceTypesPages(instanceTypesInput, func(page *ec2.DescribeInstanceTypesOutput, lastPage bool) bool {
		for _, instanceTypeInfo := range page.InstanceTypes {
			instanceTypeName := *instanceTypeInfo.InstanceType
			instanceTypeCandidates[instanceTypeName] = instanceTypeInfo
			isFpga := instanceTypeInfo.FpgaInfo != nil

			// filterToInstanceSpecMappingPairs is a map of filter name [key] to filter pair [value].
			// A filter pair includes user input filter value and instance spec value retrieved from DescribeInstanceTypes
			filterToInstanceSpecMappingPairs := map[string]filterPair{
				cpuArchitecture:        {filters.CPUArchitecture, instanceTypeInfo.ProcessorInfo.SupportedArchitectures},
				usageClass:             {filters.UsageClass, instanceTypeInfo.SupportedUsageClasses},
				rootDeviceType:         {filters.RootDeviceType, instanceTypeInfo.SupportedRootDeviceTypes},
				hibernationSupported:   {filters.HibernationSupported, instanceTypeInfo.HibernationSupported},
				vcpusRange:             {filters.VCpusRange, instanceTypeInfo.VCpuInfo.DefaultVCpus},
				memoryRange:            {filters.MemoryRange, instanceTypeInfo.MemoryInfo.SizeInMiB},
				gpuMemoryRange:         {filters.GpuMemoryRange, getTotalGpuMemory(instanceTypeInfo.GpuInfo)},
				gpusRange:              {filters.GpusRange, getTotalGpusCount(instanceTypeInfo.GpuInfo)},
				placementGroupStrategy: {filters.PlacementGroupStrategy, instanceTypeInfo.PlacementGroupInfo.SupportedStrategies},
				hypervisor:             {filters.Hypervisor, instanceTypeInfo.Hypervisor},
				baremetal:              {filters.BareMetal, instanceTypeInfo.BareMetal},
				burstable:              {filters.Burstable, instanceTypeInfo.BurstablePerformanceSupported},
				fpga:                   {filters.Fpga, &isFpga},
				enaSupport:             {filters.EnaSupport, supportSyntaxToBool(instanceTypeInfo.NetworkInfo.EnaSupport)},
				vcpusToMemoryRatio:     {filters.VCpusToMemoryRatio, calculateVCpusToMemoryRatio(instanceTypeInfo.VCpuInfo.DefaultVCpus, instanceTypeInfo.MemoryInfo.SizeInMiB)},
				currentGeneration:      {filters.CurrentGeneration, instanceTypeInfo.CurrentGeneration},
				networkInterfaces:      {filters.NetworkInterfaces, instanceTypeInfo.NetworkInfo.MaximumNetworkInterfaces},
				networkPerformance:     {filters.NetworkPerformance, getNetworkPerformance(instanceTypeInfo.NetworkInfo.NetworkPerformance)},
			}

			if !isSupportedInLocation(locationInstanceOfferings, instanceTypeName) {
				delete(instanceTypeCandidates, instanceTypeName)
			}

			var isInstanceSupported bool
			isInstanceSupported, innerErr = itf.executeFilters(filterToInstanceSpecMappingPairs, instanceTypeName)
			if innerErr != nil {
				// stops paging through instance types
				return false
			}
			if !isInstanceSupported {
				delete(instanceTypeCandidates, instanceTypeName)
			}
		}
		// continue paging through instance types
		return true
	})
	if err != nil {
		return nil, err
	}
	if innerErr != nil {
		return nil, innerErr
	}

	instanceTypeInfoSlice := []*ec2.InstanceTypeInfo{}
	for _, instanceTypeInfo := range instanceTypeCandidates {
		instanceTypeInfoSlice = append(instanceTypeInfoSlice, instanceTypeInfo)
	}
	return sortInstanceTypeInfo(instanceTypeInfoSlice), nil
}

// sortInstanceTypeInfo will sort based on instance type info alpha-numerically
func sortInstanceTypeInfo(instanceTypeInfoSlice []*ec2.InstanceTypeInfo) []*ec2.InstanceTypeInfo {
	sort.Slice(instanceTypeInfoSlice, func(i, j int) bool {
		iInstanceInfo := instanceTypeInfoSlice[i]
		jInstanceInfo := instanceTypeInfoSlice[j]
		return strings.Compare(*iInstanceInfo.InstanceType, *jInstanceInfo.InstanceType) <= 0
	})
	return instanceTypeInfoSlice
}

// executeFilters accepts a mapping of filter name to filter pairs which are iterated through
// to determine if the instance type matches the filter values.
func (itf Selector) executeFilters(filterToInstanceSpecMapping map[string]filterPair, instanceType string) (bool, error) {
	for filterName, filterPair := range filterToInstanceSpecMapping {
		filterVal := filterPair.filterValue
		instanceSpec := filterPair.instanceSpec
		// if filter is nil, user did not specify a filter, so skip evaluation
		if reflect.ValueOf(filterVal).IsNil() {
			continue
		}
		instanceSpecType := reflect.ValueOf(instanceSpec).Type()
		filterType := reflect.ValueOf(filterVal).Type()
		filterDetailsMsg := fmt.Sprintf("filter (%s: %s => %s) corresponding to instance spec (%s => %s) for instance type %s", filterName, filterVal, filterType, instanceSpec, instanceSpecType, instanceType)
		invalidInstanceSpecTypeMsg := fmt.Sprintf("Unable to process for %s", filterDetailsMsg)

		// Determine appropriate filter comparator by switching on filter type
		switch filter := filterVal.(type) {
		case *string:
			switch iSpec := instanceSpec.(type) {
			case []*string:
				if !isSupportedFromStrings(iSpec, filter) {
					return false, nil
				}
			case *string:
				if !isSupportedFromString(iSpec, filter) {
					return false, nil
				}
			default:
				return false, fmt.Errorf(invalidInstanceSpecTypeMsg)
			}
		case *bool:
			switch iSpec := instanceSpec.(type) {
			case *bool:
				if !isSupportedWithBool(iSpec, filter) {
					return false, nil
				}
			default:
				return false, fmt.Errorf(invalidInstanceSpecTypeMsg)
			}
		case *IntRangeFilter:
			switch iSpec := instanceSpec.(type) {
			case *int64:
				if !isSupportedWithRangeInt64(iSpec, filter) {
					return false, nil
				}
			case *int:
				if !isSupportedWithRangeInt(iSpec, filter) {
					return false, nil
				}
			default:
				return false, fmt.Errorf(invalidInstanceSpecTypeMsg)
			}
		case *float64:
			switch iSpec := instanceSpec.(type) {
			case *float64:
				if !isSupportedWithFloat64(iSpec, filter) {
					return false, nil
				}
			default:
				return false, fmt.Errorf(invalidInstanceSpecTypeMsg)
			}
		default:
			return false, fmt.Errorf("No filter handler found for %s", filterDetailsMsg)
		}
	}
	return true, nil
}

// RetrieveInstanceTypesSupportedInLocation returns a map of instance type -> AZ or Region for all instance types supported in the location passed in
// The location can be a zone-id (ie. use1-az1), a zone-name (us-east-1a), or a region name (us-east-1).
// Note that zone names are not necessarily the same across accounts
func (itf Selector) RetrieveInstanceTypesSupportedInLocation(zone string) (map[string]string, error) {
	if zone == "" {
		return nil, nil
	}
	availableInstanceTypes := map[string]string{}
	instanceTypeOfferingsInput := &ec2.DescribeInstanceTypeOfferingsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String(locationFilterKey),
				Values: []*string{aws.String(zone)},
			},
		},
	}
	if isZoneID, _ := regexp.MatchString(zoneIDRegex, zone); isZoneID {
		instanceTypeOfferingsInput.SetLocationType(zoneIDLocationType)
	} else if isZoneName, _ := regexp.MatchString(zoneNameRegex, zone); isZoneName {
		instanceTypeOfferingsInput.SetLocationType(zoneNameLocationType)
	} else if isRegion, _ := regexp.MatchString(regionNameRegex, zone); isRegion {
		instanceTypeOfferingsInput.SetLocationType(regionNameLocationType)
	} else {
		return nil, fmt.Errorf("The location passed in (%s) is not a valid zone-id, zone-name, or region name", zone)
	}
	err := itf.EC2.DescribeInstanceTypeOfferingsPages(instanceTypeOfferingsInput, func(page *ec2.DescribeInstanceTypeOfferingsOutput, lastPage bool) bool {
		for _, instanceType := range page.InstanceTypeOfferings {
			availableInstanceTypes[*instanceType.InstanceType] = *instanceType.Location
		}
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("Encountered an error when describing instance type offerings: %w", err)
	}
	return availableInstanceTypes, nil
}

func isSupportedInLocation(instanceOfferings map[string]string, instanceType string) bool {
	if instanceOfferings == nil {
		return true
	}
	_, ok := instanceOfferings[instanceType]
	return ok
}
