package utils

import "os"

// GetEnv returns the value of the environment variable with the given key,
// or the default value if the key is not set
func GetEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
