//go:build ignore

// A static test executable using the compiler's real sandbox launcher. Synthetic
// canaries only; it never reads host configuration or contacts public endpoints.
package main

import (
	"encoding/json"
	"net"
	"os"
	"time"
)

func main() {
	var input struct{ Canary, Endpoint string }
	body, err := os.ReadFile(os.Args[len(os.Args)-2])
	if err != nil {
		panic(err)
	}
	if err = json.Unmarshal(body, &input); err != nil {
		panic(err)
	}
	_, canaryErr := os.ReadFile(input.Canary)
	connection, networkErr := net.DialTimeout("tcp", input.Endpoint, 150*time.Millisecond)
	if connection != nil {
		connection.Close()
	}
	namespace, _ := os.Readlink("/proc/self/ns/net")
	result, _ := json.Marshal(map[string]any{"canReadCanary": canaryErr == nil, "canReachHost": networkErr == nil, "networkNamespace": namespace, "hostEnvironment": os.Getenv("RIN_SYNTHETIC_HOST_CANARY")})
	if err = os.WriteFile(os.Args[len(os.Args)-1], result, 0600); err != nil {
		panic(err)
	}
}
