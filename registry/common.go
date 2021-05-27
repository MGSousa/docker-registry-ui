package registry

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"time"

	log "github.com/sirupsen/logrus"
)

// SetupLogging setup logger
func SetupLogging(name string) *log.Entry {
	log.SetFormatter(&log.TextFormatter{
		TimestampFormat: time.RFC3339,
		FullTimestamp:   true,
	})
	// Output to stdout instead of the default stderr.
	log.SetOutput(os.Stdout)

	return log.WithFields(log.Fields{"logger": name})
}

// SortedMapKeys sort keys of the map where values can be of any type.
func SortedMapKeys(m interface{}) []string {
	v := reflect.ValueOf(m)
	keys := make([]string, 0, len(v.MapKeys()))
	for _, key := range v.MapKeys() {
		keys = append(keys, key.String())
	}
	sort.Strings(keys)
	return keys
}

// PrettySize format bytes in more readable units.
func PrettySize(size float64) string {
	var (
		units       = []string{"B", "KB", "MB", "GB"}
		decimals, i int
	)
	for size > 1024 && i < len(units) {
		size = size / 1024
		if i = i + 1; i == len(units) {
			i = i - 1
		}
	}
	// Format decimals as follow: 0 B, 0 KB, 0.0 MB, 0.00 GB
	if decimals = i - 1; decimals < 0 {
		decimals = 0
	}
	return fmt.Sprintf("%.*f %s", decimals, size, units[i])
}

// ItemInSlice check if item is an element of slice
func ItemInSlice(item string, slice []string) bool {
	for _, i := range slice {
		if i == item {
			return true
		}
	}
	return false
}
