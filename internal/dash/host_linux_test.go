//go:build linux

package dash

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

const hostTestMeminfo = `MemTotal:       10000 kB
MemFree:         1000 kB
MemAvailable:    9000 kB
Buffers:          500 kB
Cached:          2000 kB
SwapCached:       100 kB
Shmem:            200 kB
SReclaimable:     500 kB
SwapTotal:       2000 kB
SwapFree:         500 kB
HugePages_Total:    2
`

type hostTestCPURow struct {
	name   string
	fields [hostCPUFieldCount]uint64
}

func hostTestCPUStat(rows ...hostTestCPURow) string {
	var out strings.Builder
	for _, row := range rows {
		fmt.Fprintf(&out, "%s", row.name)
		for _, value := range row.fields {
			fmt.Fprintf(&out, " %d", value)
		}
		out.WriteString(" 0 0\n")
	}
	out.WriteString("intr 12345\nctxt 67890\n")
	return out.String()
}

func hostTestWriteProc(t *testing.T, dir, stat string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "meminfo"), []byte(hostTestMeminfo), 0o600))
}

func TestLinuxHostCollectorUsesBaselineDeltasAndHtopFormulas(t *testing.T) {
	dir := t.TempDir()
	baseline := hostTestCPUStat(
		hostTestCPURow{name: "cpu", fields: [8]uint64{100, 0, 100, 800}},
		hostTestCPURow{name: "cpu0", fields: [8]uint64{50, 0, 50, 400}},
		hostTestCPURow{name: "cpu1", fields: [8]uint64{50, 0, 50, 400}},
	)
	hostTestWriteProc(t, dir, baseline)
	var statRoot string
	collector := &linuxHostCollector{
		procDir: dir, root: "/",
		hostname: func() (string, error) { return "host\n\x07" + strings.Repeat("x", 300), nil },
		statfs: func(root string, stat *syscall.Statfs_t) error {
			statRoot = root
			stat.Blocks, stat.Bfree, stat.Bavail, stat.Bsize = 1000, 250, 200, 4096
			return nil
		},
	}

	first, err := collector.sample()
	require.NoError(t, err)
	require.Nil(t, first.CPU, "the first sample establishes a baseline only")
	require.Equal(t, uint64(6200*1024), first.Memory.Used)
	require.Equal(t, uint64(10000*1024), first.Memory.Total)
	require.NotNil(t, first.Swap)
	require.Equal(t, uint64(1400*1024), first.Swap.Used)
	require.Equal(t, uint64(2000*1024), first.Swap.Total)
	require.Equal(t, HostUsageSnapshot{Used: 750 * 4096, Total: 1000 * 4096}, first.Disk)
	require.Equal(t, "/", statRoot)
	require.LessOrEqual(t, utf8.RuneCountInString(first.Name), maxIDText)
	require.NotContains(t, first.Name, "\n")
	require.NotContains(t, first.Name, "\x07")

	secondStat := hostTestCPUStat(
		hostTestCPURow{name: "cpu", fields: [8]uint64{200, 0, 150, 900}},
		hostTestCPURow{name: "cpu0", fields: [8]uint64{75, 0, 75, 500}},
		hostTestCPURow{name: "cpu1", fields: [8]uint64{125, 0, 75, 400}},
	)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stat"), []byte(secondStat), 0o600))
	second, err := collector.sample()
	require.NoError(t, err)
	require.NotNil(t, second.CPU)
	require.InDelta(t, 60, second.CPU.UsedPct, 0.000001)
	require.Len(t, second.CPU.Cores, 2)
	require.Zero(t, second.CPU.CoresOmitted)
	require.Equal(t, 0, second.CPU.Cores[0].ID)
	require.InDelta(t, 100.0/3.0, second.CPU.Cores[0].UsedPct, 0.000001)
	require.Equal(t, 1, second.CPU.Cores[1].ID)
	require.InDelta(t, 100, second.CPU.Cores[1].UsedPct, 0.000001)
}

func TestHostCPURequiresMatchingTopologyAndProgress(t *testing.T) {
	baseline, err := parseHostCPUSample(hostTestCPUStat(
		hostTestCPURow{name: "cpu", fields: [8]uint64{10, 0, 10, 80}},
		hostTestCPURow{name: "cpu0", fields: [8]uint64{10, 0, 10, 80}},
	))
	require.NoError(t, err)

	for _, tc := range []struct {
		name    string
		current hostCPUSample
	}{
		{
			name: "zero aggregate delta",
			current: hostCPUSample{aggregate: baseline.aggregate, cores: []hostCPUTicks{{
				id: 0, fields: [8]uint64{11, 0, 10, 80},
			}}},
		},
		{
			name:    "aggregate counter reset",
			current: hostCPUSample{aggregate: hostCPUTicks{id: -1, fields: [8]uint64{9, 0, 10, 80}}, cores: baseline.cores},
		},
		{
			name: "core counter reset",
			current: hostCPUSample{
				aggregate: hostCPUTicks{id: -1, fields: [8]uint64{20, 0, 20, 90}},
				cores:     []hostCPUTicks{{id: 0, fields: [8]uint64{9, 0, 10, 80}}},
			},
		},
		{
			name: "missing core",
			current: hostCPUSample{
				aggregate: hostCPUTicks{id: -1, fields: [8]uint64{20, 0, 20, 90}},
			},
		},
		{
			name: "changed core identifier",
			current: hostCPUSample{
				aggregate: hostCPUTicks{id: -1, fields: [8]uint64{20, 0, 20, 90}},
				cores:     []hostCPUTicks{{id: 1, fields: [8]uint64{20, 0, 20, 90}}},
			},
		},
		{
			name: "zero core delta",
			current: hostCPUSample{
				aggregate: hostCPUTicks{id: -1, fields: [8]uint64{20, 0, 20, 90}},
				cores:     baseline.cores,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Nil(t, hostCPUUsage(tc.current, baseline))
		})
	}
}

func TestHostCollectorRebasesAfterTopologyChange(t *testing.T) {
	dir := t.TempDir()
	first := hostTestCPUStat(
		hostTestCPURow{name: "cpu", fields: [8]uint64{10, 0, 0, 10}},
		hostTestCPURow{name: "cpu0", fields: [8]uint64{10, 0, 0, 10}},
	)
	hostTestWriteProc(t, dir, first)
	collector := &linuxHostCollector{
		procDir: dir, root: "/", hostname: func() (string, error) { return "host", nil },
		statfs: func(_ string, stat *syscall.Statfs_t) error {
			stat.Blocks, stat.Bsize = 1, 1
			return nil
		},
	}
	_, err := collector.sample()
	require.NoError(t, err)

	changed := hostTestCPUStat(
		hostTestCPURow{name: "cpu", fields: [8]uint64{20, 0, 0, 20}},
		hostTestCPURow{name: "cpu2", fields: [8]uint64{20, 0, 0, 20}},
	)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stat"), []byte(changed), 0o600))
	sample, err := collector.sample()
	require.NoError(t, err)
	require.Nil(t, sample.CPU)

	progressed := hostTestCPUStat(
		hostTestCPURow{name: "cpu", fields: [8]uint64{30, 0, 0, 20}},
		hostTestCPURow{name: "cpu2", fields: [8]uint64{30, 0, 0, 20}},
	)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stat"), []byte(progressed), 0o600))
	sample, err = collector.sample()
	require.NoError(t, err)
	require.NotNil(t, sample.CPU)
	require.Equal(t, 2, sample.CPU.Cores[0].ID)
	require.InDelta(t, 100, sample.CPU.UsedPct, 0.000001)
}

func TestHostCPUCapsCoresAndReportsOmissions(t *testing.T) {
	baselineRows := make([]hostTestCPURow, 0, maxHostCores+45)
	currentRows := make([]hostTestCPURow, 0, maxHostCores+45)
	baselineRows = append(baselineRows, hostTestCPURow{name: "cpu", fields: [8]uint64{1, 0, 0, 1}})
	currentRows = append(currentRows, hostTestCPURow{name: "cpu", fields: [8]uint64{2, 0, 0, 1}})
	for i := range maxHostCores + 44 {
		name := fmt.Sprintf("cpu%d", i)
		baselineRows = append(baselineRows, hostTestCPURow{name: name, fields: [8]uint64{1, 0, 0, 1}})
		currentRows = append(currentRows, hostTestCPURow{name: name, fields: [8]uint64{2, 0, 0, 1}})
	}
	baseline, err := parseHostCPUSample(hostTestCPUStat(baselineRows...))
	require.NoError(t, err)
	current, err := parseHostCPUSample(hostTestCPUStat(currentRows...))
	require.NoError(t, err)
	usage := hostCPUUsage(current, baseline)
	require.NotNil(t, usage)
	require.Len(t, usage.Cores, maxHostCores)
	require.Equal(t, 44, usage.CoresOmitted)
	require.Equal(t, 0, usage.Cores[0].ID)
	require.Equal(t, maxHostCores-1, usage.Cores[len(usage.Cores)-1].ID)
}

func TestParseHostMemoryOmitsZeroSwap(t *testing.T) {
	data := strings.Replace(hostTestMeminfo, "SwapTotal:       2000 kB", "SwapTotal:          0 kB", 1)
	data = strings.Replace(data, "SwapFree:         500 kB", "SwapFree:           0 kB", 1)
	memory, swap, err := parseHostMemory(data)
	require.NoError(t, err)
	require.Equal(t, uint64(6200*1024), memory.Used)
	require.Nil(t, swap)
}

func TestParseHostMemoryRejectsMalformedRequiredFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "missing", data: strings.Replace(hostTestMeminfo, "MemFree:         1000 kB\n", "", 1)},
		{name: "duplicate", data: hostTestMeminfo + "MemFree: 1 kB\n"},
		{name: "wrong unit", data: strings.Replace(hostTestMeminfo, "MemFree:         1000 kB", "MemFree: 1000 MB", 1)},
		{name: "negative", data: strings.Replace(hostTestMeminfo, "MemFree:         1000 kB", "MemFree: -1 kB", 1)},
		{name: "missing value", data: strings.Replace(hostTestMeminfo, "MemFree:         1000 kB", "MemFree: kB", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := parseHostMemory(tc.data)
			require.Error(t, err)
		})
	}
}

func TestParseHostCPURejectsMalformedTopologyAndCounters(t *testing.T) {
	validCore := hostTestCPURow{name: "cpu0", fields: [8]uint64{1, 0, 0, 1}}
	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "empty", data: ""},
		{name: "core without aggregate", data: hostTestCPUStat(validCore)},
		{name: "aggregate without core", data: hostTestCPUStat(hostTestCPURow{name: "cpu", fields: [8]uint64{1}})},
		{name: "duplicate aggregate", data: hostTestCPUStat(
			hostTestCPURow{name: "cpu", fields: [8]uint64{1}}, hostTestCPURow{name: "cpu", fields: [8]uint64{1}}, validCore,
		)},
		{name: "duplicate core", data: hostTestCPUStat(
			hostTestCPURow{name: "cpu", fields: [8]uint64{1}}, validCore, validCore,
		)},
		{name: "invalid identifier", data: "cpu 1 0 0 1 0 0 0 0\ncpux 1 0 0 1 0 0 0 0\n"},
		{name: "short row", data: "cpu 1 0 0\ncpu0 1 0 0\n"},
		{name: "negative counter", data: "cpu -1 0 0 1 0 0 0 0\ncpu0 1 0 0 1 0 0 0 0\n"},
		{name: "overflow", data: fmt.Sprintf("cpu %d %d 0 0 0 0 0 0\ncpu0 1 0 0 1 0 0 0 0\n", uint64(math.MaxUint64), uint64(1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseHostCPUSample(tc.data)
			require.Error(t, err)
		})
	}
}

func TestHostArithmeticAndDiskUsageCannotWrap(t *testing.T) {
	_, ok := checkedHostAdd(math.MaxUint64, 1)
	require.False(t, ok)
	require.Equal(t, uint64(math.MaxUint64), saturatingHostAdd(math.MaxUint64, 1))
	require.Equal(t, uint64(math.MaxUint64), saturatingHostMultiply(math.MaxUint64, 2))
	require.Zero(t, saturatingHostSubtract(10, 11))

	usage, err := hostDiskUsage(&syscall.Statfs_t{Blocks: math.MaxUint64, Bsize: 4096})
	require.NoError(t, err)
	require.Equal(t, uint64(math.MaxUint64), usage.Total)
	require.Equal(t, uint64(math.MaxUint64), usage.Used)

	usage, err = hostDiskUsage(&syscall.Statfs_t{Blocks: 100, Bfree: 150, Bsize: 10})
	require.NoError(t, err)
	require.Equal(t, HostUsageSnapshot{Used: 0, Total: 1000}, usage)
	_, err = hostDiskUsage(&syscall.Statfs_t{Blocks: 1, Bsize: 0})
	require.Error(t, err)
}
