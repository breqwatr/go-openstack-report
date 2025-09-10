package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/snapshots"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
)

var flavorRegex = regexp.MustCompile(`v\d+\.c(\d+)r(\d+)`)

type FixedServer struct {
	servers.Server
	Addresses map[string]interface{} `json:"addresses"`
}

type PricingConfig struct {
	FloatingIPPrice float64            // per IP per month
	StoragePrice    float64            // per GB per month
	MemoryPrice     float64            // per GB per month
	LicensePrices   map[string]float64 // license type -> price per month
	FlavorPrices    map[string]float64 // flavor name -> additional price per month
}

type Report struct {
	VMName                string
	Flavor                string
	License               string
	VMID                  string
	DiskSizes             []int
	FloatingIPs           []string
	MonthlyStorageCost    float64
	MonthlyMemoryCost     float64
	MonthlyLicenseCost    float64
	MonthlyFloatingIPCost float64
	TotalMonthlyCost      float64
}

type Summary struct {
	VMCount             int
	TotalStorage        int
	UnallocatedStorage  int
	TotalFloatingIPs    int
	TotalSnapshots      int
	TotalSnapshotSize   int
	LicenseCounts       map[string]int
	TotalVCPUs          int
	TotalRAM            int
	TotalMonthlyCost    float64
	TotalStorageCost    float64
	TotalMemoryCost     float64
	TotalLicenseCost    float64
	TotalFloatingIPCost float64
}

func main() {
	ctx := context.Background()

	opts, err := openstack.AuthOptionsFromEnv()
	if err != nil {
		log.Fatalf("Failed to get auth options: %v", err)
	}

	if os.Getenv("OS_PROJECT_DOMAIN_ID") != "" {
		opts.DomainID = os.Getenv("OS_PROJECT_DOMAIN_ID")
	} else {
		opts.DomainName = os.Getenv("OS_PROJECT_DOMAIN_NAME")
	}

	provider, err := openstack.AuthenticatedClient(ctx, opts)
	if err != nil {
		log.Fatalf("Failed to authenticate: %v", err)
	}

	computeClient, err := openstack.NewComputeV2(provider, gophercloud.EndpointOpts{})
	if err != nil {
		log.Fatalf("Failed to create compute client: %v", err)
	}

	storageClient, err := openstack.NewBlockStorageV3(provider, gophercloud.EndpointOpts{})
	if err != nil {
		log.Fatalf("Failed to create storage client: %v", err)
	}

	networkClient, err := openstack.NewNetworkV2(provider, gophercloud.EndpointOpts{})
	if err != nil {
		log.Fatalf("Failed to create network client: %v", err)
	}

	allVMs, _ := listFixedServers(ctx, computeClient)
	allFlavors, _ := listFlavors(ctx, computeClient)
	allPorts, _ := listPorts(ctx, networkClient)
	allVolumes, _ := listVolumes(ctx, storageClient)
	allFIPs, _ := listFloatingIPs(ctx, networkClient)
	allSnapshots, _ := listSnapshots(ctx, storageClient)

	flavorMap := make(map[string]string)
	for _, flavor := range allFlavors {
		flavorMap[flavor.ID] = flavor.Name
	}

	portToFIP := make(map[string][]string)
	for _, fip := range allFIPs {
		if fip.PortID != "" {
			portToFIP[fip.PortID] = append(portToFIP[fip.PortID], fip.FloatingIP)
		}
	}

	pricing := PricingConfig{
		FloatingIPPrice: 5.00,  // $5 per floating IP per month
		StoragePrice:    0.10,  // $0.10 per GB storage per month
		MemoryPrice:     10.00, // $10.00 per GB RAM per month
		LicensePrices: map[string]float64{
			"windows": 8.00,   // $8 per 2-core bundle for Windows
			"sql":     230.00, // $230 per 2-core bundle for SQL Server
		},
		FlavorPrices: map[string]float64{
			// Add specific flavor surcharges if needed
			"windows.large": 25.00, // Additional $25 for large Windows VMs
		},
	}

	unallocatedStorage, unallocatedDisks, unallocatedDiskSizes := calculateUnallocatedStorage(allVolumes)
	report := generateReport(allVMs, allPorts, allVolumes, portToFIP, flavorMap, pricing)

	summaryData := generateSummary(report, unallocatedStorage, allSnapshots)
	writeExcel(report, unallocatedStorage, unallocatedDisks, unallocatedDiskSizes, summaryData)
}

func listFixedServers(ctx context.Context, client *gophercloud.ServiceClient) ([]FixedServer, error) {
	allPages, err := servers.List(client, nil).AllPages(ctx)
	if err != nil {
		return nil, err
	}

	var allServers []FixedServer
	err = servers.ExtractServersInto(allPages, &allServers)
	return allServers, err
}

func listFlavors(ctx context.Context, client *gophercloud.ServiceClient) ([]flavors.Flavor, error) {
	allPages, err := flavors.ListDetail(client, nil).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	return flavors.ExtractFlavors(allPages)
}

func listPorts(ctx context.Context, client *gophercloud.ServiceClient) ([]ports.Port, error) {
	allPages, err := ports.List(client, nil).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	return ports.ExtractPorts(allPages)
}

func listVolumes(ctx context.Context, client *gophercloud.ServiceClient) ([]volumes.Volume, error) {
	allPages, err := volumes.List(client, nil).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	return volumes.ExtractVolumes(allPages)
}

func listFloatingIPs(ctx context.Context, client *gophercloud.ServiceClient) ([]floatingips.FloatingIP, error) {
	allPages, err := floatingips.List(client, nil).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	return floatingips.ExtractFloatingIPs(allPages)
}

func listSnapshots(ctx context.Context, client *gophercloud.ServiceClient) ([]snapshots.Snapshot, error) {
	allPages, err := snapshots.List(client, nil).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	return snapshots.ExtractSnapshots(allPages)
}

func calculateUnallocatedStorage(vols []volumes.Volume) (int, []string, []int) {
	totalUnallocated := 0
	var unallocatedDisks []string
	var unallocatedDiskSizes []int

	for _, vol := range vols {
		if len(vol.Attachments) == 0 {
			totalUnallocated += vol.Size
			unallocatedDisks = append(unallocatedDisks, vol.Name)
			unallocatedDiskSizes = append(unallocatedDiskSizes, vol.Size)
		}
	}
	return totalUnallocated, unallocatedDisks, unallocatedDiskSizes
}

// generateReport identifies boot disks and sorts extra volumes.
func generateReport(vms []FixedServer, ports []ports.Port, vols []volumes.Volume, portToFIP map[string][]string, flavorMap map[string]string, pricing PricingConfig) []Report {
	var report []Report

	for _, vm := range vms {
		licenseType := ""
		if meta, exists := vm.Metadata["license_type"]; exists {
			licenseType = fmt.Sprintf("%v", meta)
		}

		flavor := flavorMap[vm.Flavor["id"].(string)]
		var diskSizes []int
		var floatingIPs []string
		var bootDiskSize int
		var extraDiskSizes []int

		var attachedVolumes []volumes.Volume

		for _, vol := range vols {
			for _, attachment := range vol.Attachments {
				if attachment.ServerID == vm.ID {
					attachedVolumes = append(attachedVolumes, vol)
				}
			}
		}

		if len(attachedVolumes) > 0 {
			sort.Slice(attachedVolumes, func(i, j int) bool {
				return attachedVolumes[i].Name < attachedVolumes[j].Name
			})

			bootDiskSize = attachedVolumes[0].Size
			for i, vol := range attachedVolumes {
				if i == 0 {
					continue
				}
				extraDiskSizes = append(extraDiskSizes, vol.Size)
			}
		}

		if bootDiskSize > 0 {
			diskSizes = append(diskSizes, bootDiskSize)
		}

		sort.Ints(extraDiskSizes)
		diskSizes = append(diskSizes, extraDiskSizes...)

		for _, port := range ports {
			if port.DeviceID == vm.ID {
				if ips, exists := portToFIP[port.ID]; exists {
					floatingIPs = append(floatingIPs, ips...)
				}
			}
		}

		// Calculate costs
		totalStorage := sum(diskSizes)
		storageCost := float64(totalStorage) * pricing.StoragePrice

		memoryCost := 0.0
		licenseCost := 0.0
		cores := 0

		// Extract cores and memory from flavor regex
		if matches := flavorRegex.FindStringSubmatch(flavor); len(matches) == 3 {
			if c, err := strconv.Atoi(matches[1]); err == nil {
				cores = c
			}
			if ram, err := strconv.Atoi(matches[2]); err == nil {
				memoryCost = float64(ram) * pricing.MemoryPrice
			}
		}

		// Calculate license cost based on 2-core bundles, minimum 4 cores
		if cores > 0 {
			licenseCores := cores
			if licenseCores < 4 {
				licenseCores = 4 // Minimum 4 cores for licensing
			}
			coreBundles := (licenseCores + 1) / 2 // Round up to nearest 2-core bundle
			
			licenseKey := strings.ToLower(licenseType)
			// Check for license matches (including partial matches)
			for licensePattern, pricePerBundle := range pricing.LicensePrices {
				if strings.Contains(licenseKey, licensePattern) {
					licenseCost = float64(coreBundles) * pricePerBundle
					break
				}
			}
		}

		floatingIPCost := float64(len(floatingIPs)) * pricing.FloatingIPPrice
		flavorCost := pricing.FlavorPrices[flavor] // Additional flavor-based costs

		totalCost := storageCost + memoryCost + licenseCost + floatingIPCost + flavorCost

		report = append(report, Report{
			VMID:                  vm.ID,
			VMName:                vm.Name,
			Flavor:                flavor,
			License:               licenseType,
			DiskSizes:             diskSizes,
			FloatingIPs:           floatingIPs,
			MonthlyStorageCost:    storageCost,
			MonthlyMemoryCost:     memoryCost,
			MonthlyLicenseCost:    licenseCost,
			MonthlyFloatingIPCost: floatingIPCost,
			TotalMonthlyCost:      totalCost,
		})
	}
	return report
}

// generateSummary generates a summary of the report including total storage, floating IPs, and license counts.
func generateSummary(report []Report, unallocatedStorage int, allSnapshots []snapshots.Snapshot) Summary {
	summary := Summary{
		VMCount:             len(report),
		TotalStorage:        0,
		UnallocatedStorage:  unallocatedStorage,
		TotalFloatingIPs:    0,
		TotalSnapshots:      len(allSnapshots),
		TotalSnapshotSize:   0,
		LicenseCounts:       make(map[string]int),
		TotalVCPUs:          0,
		TotalRAM:            0,
		TotalMonthlyCost:    0,
		TotalStorageCost:    0,
		TotalMemoryCost:     0,
		TotalLicenseCost:    0,
		TotalFloatingIPCost: 0,
	}

	// Loop through each VM in the report
	for _, entry := range report {
		summary.TotalStorage += sum(entry.DiskSizes)
		summary.TotalFloatingIPs += len(entry.FloatingIPs)

		// Sum up costs
		summary.TotalMonthlyCost += entry.TotalMonthlyCost
		summary.TotalStorageCost += entry.MonthlyStorageCost
		summary.TotalMemoryCost += entry.MonthlyMemoryCost
		summary.TotalLicenseCost += entry.MonthlyLicenseCost
		summary.TotalFloatingIPCost += entry.MonthlyFloatingIPCost

		// Count licenses
		license := entry.License
		if license == "" || license == "null" {
			license = "Generic OS"
		}
		summary.LicenseCounts[license]++

		matches := flavorRegex.FindStringSubmatch(entry.Flavor)
		if len(matches) == 3 {
			vCPUs, _ := strconv.Atoi(matches[1])
			RAM, _ := strconv.Atoi(matches[2])
			summary.TotalVCPUs += vCPUs
			summary.TotalRAM += RAM
		}
	}

	// Calculate total snapshot size
	for _, snap := range allSnapshots {
		summary.TotalSnapshotSize += snap.Size
	}

	return summary
}

func sum(arr []int) int {
	total := 0
	for _, v := range arr {
		total += v
	}
	return total
}

func excelColumn(n int) string {
	letters := ""
	for n > 0 {
		n--
		letters = fmt.Sprintf("%c", 'A'+(n%26)) + letters
		n /= 26
	}
	return letters
}

func writeExcel(report []Report, unallocatedStorage int, unallocatedDisks []string, unallocatedDiskSizes []int, summaryData Summary) {
	timestamp := time.Now().Format("2006-01-02")
	filename := fmt.Sprintf("report-%s.xlsx", timestamp)

	f := excelize.NewFile()

	createSummarySheet(f, summaryData, unallocatedStorage)
	createVMReportSheet(f, report)
	createUnallocatedDisksSheet(f, unallocatedDisks, unallocatedDiskSizes)

	// Delete the default "Sheet1"
	if err := f.DeleteSheet("Sheet1"); err != nil {
		log.Fatalf("Failed to delete default sheet: %v", err)
	}

	if err := f.SaveAs(filename); err != nil {
		log.Fatalf("Failed to save Excel file: %v", err)
	}
}

func createSummarySheet(f *excelize.File, summaryData Summary, unallocatedStorage int) {
	summarySheet := "Summary"
	summaryIndex, err := f.NewSheet(summarySheet)
	if err != nil {
		log.Fatalf("Failed to create summary sheet: %v", err)
	}

	f.SetCellValue(summarySheet, "A1", "Summary Report")
	f.SetCellValue(summarySheet, "A3", "Total VM Count")
	f.SetCellValue(summarySheet, "B3", summaryData.VMCount)
	f.SetCellValue(summarySheet, "A4", "Total Storage (GB)")
	f.SetCellValue(summarySheet, "B4", summaryData.TotalStorage)
	f.SetCellValue(summarySheet, "A5", "Unallocated Storage (GB)")
	f.SetCellValue(summarySheet, "B5", unallocatedStorage)
	f.SetCellValue(summarySheet, "A6", "Total Snapshots")
	f.SetCellValue(summarySheet, "B6", summaryData.TotalSnapshots)
	f.SetCellValue(summarySheet, "A7", "Total Snapshot Size (GB)")
	f.SetCellValue(summarySheet, "B7", summaryData.TotalSnapshotSize)
	f.SetCellValue(summarySheet, "A8", "Total Floating IPs")
	f.SetCellValue(summarySheet, "B8", summaryData.TotalFloatingIPs)
	f.SetCellValue(summarySheet, "A9", "Total vCPUs")
	f.SetCellValue(summarySheet, "B9", summaryData.TotalVCPUs)
	f.SetCellValue(summarySheet, "A10", "Total RAM (GB)")
	f.SetCellValue(summarySheet, "B10", summaryData.TotalRAM)

	f.SetCellValue(summarySheet, "A12", "Total Monthly Cost")
	f.SetCellValue(summarySheet, "B12", summaryData.TotalMonthlyCost)

	f.SetCellValue(summarySheet, "D3", "Cost Breakdown")
	f.SetCellValue(summarySheet, "D4", "Storage Cost")
	f.SetCellValue(summarySheet, "E4", summaryData.TotalStorageCost)
	f.SetCellValue(summarySheet, "D5", "Memory Cost")
	f.SetCellValue(summarySheet, "E5", summaryData.TotalMemoryCost)
	f.SetCellValue(summarySheet, "D6", "License Cost")
	f.SetCellValue(summarySheet, "E6", summaryData.TotalLicenseCost)
	f.SetCellValue(summarySheet, "D7", "Floating IP Cost")
	f.SetCellValue(summarySheet, "E7", summaryData.TotalFloatingIPCost)

	// Format cost cells as USD currency
	currencyStyle, _ := f.NewStyle(&excelize.Style{
		NumFmt: 7, // USD currency format
	})
	f.SetCellStyle(summarySheet, "B12", "B12", currencyStyle)
	f.SetCellStyle(summarySheet, "E4", "E7", currencyStyle)

	f.SetCellValue(summarySheet, "A14", "License Type")
	f.SetCellValue(summarySheet, "B14", "Count")

	row := 16
	for license, count := range summaryData.LicenseCounts {
		f.SetCellValue(summarySheet, fmt.Sprintf("A%d", row), license)
		f.SetCellValue(summarySheet, fmt.Sprintf("B%d", row), count)
		row++
	}

	f.SetActiveSheet(summaryIndex)
}

func createVMReportSheet(f *excelize.File, report []Report) {
	vmSheet := "VM Report"
	_, err := f.NewSheet(vmSheet)
	if err != nil {
		log.Fatalf("Failed to create VM report sheet: %v", err)
	}

	header := []string{"VM Name", "Flavor", "License", "VM ID"}
	maxDisks := 0
	for _, entry := range report {
		if len(entry.DiskSizes) > maxDisks {
			maxDisks = len(entry.DiskSizes)
		}
	}
	for i := 1; i <= maxDisks; i++ {
		header = append(header, fmt.Sprintf("Disk %d Size (GB)", i))
	}
	header = append(header, "Floating IPs", "Storage Cost", "Memory Cost", "License Cost", "Floating IP Cost", "Total Monthly Cost")

	for col, h := range header {
		f.SetCellValue(vmSheet, excelColumn(col+1)+"1", h)
	}

	for row, entry := range report {
		rowNum := row + 2
		f.SetCellValue(vmSheet, excelColumn(1)+strconv.Itoa(rowNum), entry.VMName)
		f.SetCellValue(vmSheet, excelColumn(2)+strconv.Itoa(rowNum), entry.Flavor)
		f.SetCellValue(vmSheet, excelColumn(3)+strconv.Itoa(rowNum), entry.License)
		f.SetCellValue(vmSheet, excelColumn(4)+strconv.Itoa(rowNum), entry.VMID)

		for i, size := range entry.DiskSizes {
			f.SetCellValue(vmSheet, excelColumn(5+i)+strconv.Itoa(rowNum), size)
		}

		col := 5 + maxDisks
		f.SetCellValue(vmSheet, excelColumn(col)+strconv.Itoa(rowNum), strings.Join(entry.FloatingIPs, ", "))
		f.SetCellValue(vmSheet, excelColumn(col+1)+strconv.Itoa(rowNum), entry.MonthlyStorageCost)
		f.SetCellValue(vmSheet, excelColumn(col+2)+strconv.Itoa(rowNum), entry.MonthlyMemoryCost)
		f.SetCellValue(vmSheet, excelColumn(col+3)+strconv.Itoa(rowNum), entry.MonthlyLicenseCost)
		f.SetCellValue(vmSheet, excelColumn(col+4)+strconv.Itoa(rowNum), entry.MonthlyFloatingIPCost)
		f.SetCellValue(vmSheet, excelColumn(col+5)+strconv.Itoa(rowNum), entry.TotalMonthlyCost)
		
		// Format cost columns as USD currency
		currencyStyle, _ := f.NewStyle(&excelize.Style{
			NumFmt: 7, // USD currency format
		})
		for i := 1; i <= 5; i++ {
			f.SetCellStyle(vmSheet, excelColumn(col+i)+strconv.Itoa(rowNum), excelColumn(col+i)+strconv.Itoa(rowNum), currencyStyle)
		}
	}
}

func createUnallocatedDisksSheet(f *excelize.File, unallocatedDisks []string, unallocatedDiskSizes []int) {
	unallocatedSheet := "Unallocated Disks"
	_, err := f.NewSheet(unallocatedSheet)
	if err != nil {
		log.Fatalf("Failed to create Unallocated Disks sheet: %v", err)
	}

	f.SetCellValue(unallocatedSheet, "A1", "Unallocated Disk Name")
	f.SetCellValue(unallocatedSheet, "B1", "Size (GB)")

	for i, disk := range unallocatedDisks {
		f.SetCellValue(unallocatedSheet, fmt.Sprintf("A%d", i+2), disk)
		f.SetCellValue(unallocatedSheet, fmt.Sprintf("B%d", i+2), unallocatedDiskSizes[i])
	}
}
