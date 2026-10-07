package pulse

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/antonismor/Titanus-Core/internal/realm"
)

func Discover(stateRoot string) realm.Resources {
	resources := realm.Resources{
		CPUMilliCapacity: int64(runtime.NumCPU()) * 1000,
		GPUCount:         countGPUs(),
	}
	resources.MemoryBytes, resources.MemoryUsedBytes = memory()
	resources.CPUMilliUsed = cpuPressure(resources.CPUMilliCapacity)
	resources.UnitCount = unitCount(stateRoot)
	return resources
}

func memory() (total, used int64) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer file.Close()
	values := map[string]int64{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err == nil {
			values[strings.TrimSuffix(fields[0], ":")] = value * 1024
		}
	}
	total = values["MemTotal"]
	available := values["MemAvailable"]
	if available <= total {
		used = total - available
	}
	return total, used
}

func cpuPressure(capacity int64) int64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	used := int64(load * 1000)
	if used > capacity {
		return capacity
	}
	return used
}

func countGPUs() int {
	matches, _ := filepath.Glob("/dev/dri/renderD*")
	return len(matches)
}

func unitCount(stateRoot string) int {
	entries, err := os.ReadDir(filepath.Join(stateRoot, "units"))
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			count++
		}
	}
	return count
}
