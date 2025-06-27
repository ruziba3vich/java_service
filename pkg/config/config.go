package config

import (
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

type (
	Config struct {
		AppPort, JavaContainerName, ContainerWorkingDir, JavaSourceFileName, JavaClassName, LogPath string
		CompilationTimeout, ExecutionTimeout                                                        time.Duration
	}
)

func NewConfig() *Config {
	_ = godotenv.Load()
	return &Config{
		LogPath:             getEnv("LOG_PATH", "app.log"),
		AppPort:             getEnv("APP_PORT", "701"),
		JavaContainerName:   getEnv("JAVA_CONTAINER_NAME", "online_compiler-java-runner-1"),
		JavaClassName:       getEnv("JAVA_CLASS_NAME", "Main"),
		JavaSourceFileName:  getEnv("JAVA_SOURCE_FILE_NAME", "Main.java"),
		ContainerWorkingDir: getEnv("CONTAINER_WORKING_DIR", "/app"),
		CompilationTimeout:  getEnvTime("COMPILATION_TIME_OUT", 15),
		ExecutionTimeout:    getEnvTime("EXECUTION_TIME_OUT", 60),
	}
}

// getEnv returns the fallback value if the given key is not provided in env
func getEnv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func getEnvTime(key string, fallback int) time.Duration {
	if value := os.Getenv(key); value != "" {
		v, err := strconv.Atoi(value)
		if err == nil {
			return time.Duration(v) * time.Second
		}

	}
	return time.Duration(fallback) * time.Second
}
