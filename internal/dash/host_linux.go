//go:build linux

package dash

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const hostCPUFieldCount = 8

type hostCPUTicks struct {
	id     int
	fields [hostCPUFieldCount]uint64
}

type hostCPUSample struct {
	aggregate hostCPUTicks
	cores     []hostCPUTicks
}

type linuxHostCollector struct {
	procDir  string
	root     string
	hostname func() (string, error)
	statfs   func(string, *syscall.Statfs_t) error
	previous *hostCPUSample
}

func newHostSampler() hostSampleFunc {
	collector := &linuxHostCollector{
		procDir: "/proc", root: "/", hostname: os.Hostname, statfs: syscall.Statfs,
	}
	return collector.sample
}

func (c *linuxHostCollector) sample() (HostSnapshot, error) {
	cpuData, err := os.ReadFile(filepath.Join(c.procDir, "stat"))
	if err != nil {
		return HostSnapshot{}, fmt.Errorf("dash: read host CPU: %w", err)
	}
	cpu, err := parseHostCPUSample(string(cpuData))
	if err != nil {
		return HostSnapshot{}, err
	}
	memoryData, err := os.ReadFile(filepath.Join(c.procDir, "meminfo"))
	if err != nil {
		return HostSnapshot{}, fmt.Errorf("dash: read host memory: %w", err)
	}
	memory, swap, err := parseHostMemory(string(memoryData))
	if err != nil {
		return HostSnapshot{}, err
	}
	var stat syscall.Statfs_t
	if err := c.statfs(c.root, &stat); err != nil {
		return HostSnapshot{}, fmt.Errorf("dash: stat host root: %w", err)
	}
	disk, err := hostDiskUsage(&stat)
	if err != nil {
		return HostSnapshot{}, err
	}
	name, err := c.hostname()
	if err != nil {
		return HostSnapshot{}, fmt.Errorf("dash: read hostname: %w", err)
	}

	sample := HostSnapshot{
		Name: boundedText(name, maxIDText), Memory: memory, Swap: swap, Disk: disk,
	}
	if c.previous != nil {
		sample.CPU = hostCPUUsage(cpu, *c.previous)
	}
	c.previous = &cpu
	return sample, nil
}

func parseHostCPUSample(data string) (hostCPUSample, error) {
	var sample hostCPUSample
	seenAggregate := false
	seenCores := make(map[int]struct{})
	for line := range strings.Lines(data) {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "cpu") {
			continue
		}
		if fields[0] == "cpu" {
			if seenAggregate || len(sample.cores) != 0 {
				return hostCPUSample{}, errors.New("dash: invalid aggregate CPU topology")
			}
			ticks, err := parseHostCPUTicks(fields[1:], -1)
			if err != nil {
				return hostCPUSample{}, err
			}
			sample.aggregate = ticks
			seenAggregate = true
			continue
		}
		if !seenAggregate {
			return hostCPUSample{}, errors.New("dash: missing aggregate CPU before cores")
		}
		id, err := strconv.Atoi(strings.TrimPrefix(fields[0], "cpu"))
		if err != nil || id < 0 {
			return hostCPUSample{}, errors.New("dash: invalid CPU identifier")
		}
		if _, exists := seenCores[id]; exists {
			return hostCPUSample{}, errors.New("dash: duplicate CPU identifier")
		}
		ticks, err := parseHostCPUTicks(fields[1:], id)
		if err != nil {
			return hostCPUSample{}, err
		}
		sample.cores = append(sample.cores, ticks)
		seenCores[id] = struct{}{}
	}
	if !seenAggregate || len(sample.cores) == 0 {
		return hostCPUSample{}, errors.New("dash: incomplete CPU topology")
	}
	return sample, nil
}

func parseHostCPUTicks(fields []string, id int) (hostCPUTicks, error) {
	if len(fields) < hostCPUFieldCount {
		return hostCPUTicks{}, errors.New("dash: short CPU counter row")
	}
	var ticks hostCPUTicks
	ticks.id = id
	var total uint64
	for i := range hostCPUFieldCount {
		value, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			return hostCPUTicks{}, fmt.Errorf("dash: invalid CPU counter: %w", err)
		}
		var ok bool
		total, ok = checkedHostAdd(total, value)
		if !ok {
			return hostCPUTicks{}, errors.New("dash: CPU counter overflow")
		}
		ticks.fields[i] = value
	}
	return ticks, nil
}

func hostCPUUsage(current, previous hostCPUSample) *HostCPUSnapshot {
	if len(current.cores) != len(previous.cores) {
		return nil
	}
	aggregate, ok := hostCPUUsageDelta(current.aggregate, previous.aggregate)
	if !ok {
		return nil
	}
	usage := &HostCPUSnapshot{
		UsedPct: aggregate, Cores: make([]HostCoreSnapshot, 0, min(len(current.cores), maxHostCores)),
		CoresOmitted: max(len(current.cores)-maxHostCores, 0),
	}
	for i := range current.cores {
		if current.cores[i].id != previous.cores[i].id {
			return nil
		}
		usedPct, valid := hostCPUUsageDelta(current.cores[i], previous.cores[i])
		if !valid {
			return nil
		}
		if len(usage.Cores) < maxHostCores {
			usage.Cores = append(usage.Cores, HostCoreSnapshot{ID: current.cores[i].id, UsedPct: usedPct})
		}
	}
	return usage
}

func hostCPUUsageDelta(current, previous hostCPUTicks) (float64, bool) {
	if current.id != previous.id {
		return 0, false
	}
	var currentTotal, previousTotal, currentIdle, previousIdle uint64
	for i := range hostCPUFieldCount {
		if current.fields[i] < previous.fields[i] {
			return 0, false
		}
		currentTotal += current.fields[i]
		previousTotal += previous.fields[i]
		if i == 3 || i == 4 {
			currentIdle += current.fields[i]
			previousIdle += previous.fields[i]
		}
	}
	if currentTotal <= previousTotal || currentIdle < previousIdle {
		return 0, false
	}
	totalDelta := currentTotal - previousTotal
	idleDelta := currentIdle - previousIdle
	usedDelta := totalDelta - idleDelta
	return min(max(100*float64(usedDelta)/float64(totalDelta), 0), 100), true
}

var hostMemoryFields = map[string]struct{}{
	"MemTotal": {}, "MemFree": {}, "Buffers": {}, "Cached": {}, "SReclaimable": {},
	"Shmem": {}, "SwapTotal": {}, "SwapFree": {}, "SwapCached": {},
}

func parseHostMemory(data string) (HostUsageSnapshot, *HostUsageSnapshot, error) {
	values := make(map[string]uint64, len(hostMemoryFields))
	for line := range strings.Lines(data) {
		key, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		if _, wanted := hostMemoryFields[key]; !wanted {
			continue
		}
		if _, duplicate := values[key]; duplicate {
			return HostUsageSnapshot{}, nil, errors.New("dash: duplicate memory field")
		}
		fields := strings.Fields(rest)
		if len(fields) != 2 || fields[1] != "kB" {
			return HostUsageSnapshot{}, nil, errors.New("dash: invalid memory field")
		}
		value, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return HostUsageSnapshot{}, nil, fmt.Errorf("dash: invalid memory value: %w", err)
		}
		values[key] = saturatingHostMultiply(value, 1024)
	}
	for key := range hostMemoryFields {
		if _, found := values[key]; !found {
			return HostUsageSnapshot{}, nil, errors.New("dash: missing memory field")
		}
	}

	memoryTotal := values["MemTotal"]
	memoryFree := saturatingHostAdd(values["MemFree"], values["Buffers"])
	memoryFree = saturatingHostAdd(memoryFree, values["Cached"])
	memoryFree = saturatingHostAdd(memoryFree, values["SReclaimable"])
	memoryUsed := saturatingHostAdd(saturatingHostSubtract(memoryTotal, memoryFree), values["Shmem"])
	memory := HostUsageSnapshot{Used: min(memoryUsed, memoryTotal), Total: memoryTotal}

	if values["SwapTotal"] == 0 {
		return memory, nil, nil
	}
	swapUsed := saturatingHostSubtract(values["SwapTotal"], saturatingHostAdd(values["SwapFree"], values["SwapCached"]))
	swap := &HostUsageSnapshot{Used: min(swapUsed, values["SwapTotal"]), Total: values["SwapTotal"]}
	return memory, swap, nil
}

func hostDiskUsage(stat *syscall.Statfs_t) (HostUsageSnapshot, error) {
	if stat == nil || stat.Bsize <= 0 {
		return HostUsageSnapshot{}, errors.New("dash: invalid root filesystem block size")
	}
	blockSize := uint64(stat.Bsize)
	freeBlocks := min(stat.Bfree, stat.Blocks)
	return HostUsageSnapshot{
		Used: saturatingHostMultiply(stat.Blocks-freeBlocks, blockSize), Total: saturatingHostMultiply(stat.Blocks, blockSize),
	}, nil
}

func checkedHostAdd(left, right uint64) (uint64, bool) {
	sum := left + right
	return sum, sum >= left
}

func saturatingHostAdd(left, right uint64) uint64 {
	sum, ok := checkedHostAdd(left, right)
	if !ok {
		return ^uint64(0)
	}
	return sum
}

func saturatingHostSubtract(left, right uint64) uint64 {
	if right >= left {
		return 0
	}
	return left - right
}

func saturatingHostMultiply(left, right uint64) uint64 {
	if left != 0 && right > ^uint64(0)/left {
		return ^uint64(0)
	}
	return left * right
}
