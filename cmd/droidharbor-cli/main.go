package main

import (
	"encoding/json"
	"os"
)

func main() {
	var result any
	var failure *commandFailure
	foreground := false
	if len(os.Args) < 2 {
		failure = &commandFailure{Code: "INVALID_ARGUMENT", Message: "支持命令：version、capabilities、claim、status、renew、release、connect、env、disconnect", exitCode: 2}
	} else {
		switch os.Args[1] {
		case "version":
			if len(os.Args) != 2 {
				failure = &commandFailure{Code: "INVALID_ARGUMENT", Message: "version 不接受额外参数", exitCode: 2}
			} else {
				result = map[string]any{"product": "droidharbor-cli", "version": "0.0.0-dev", "apiMajor": 1}
			}
		case "capabilities":
			result, failure = capabilities(os.Args[2:])
		case "claim":
			result, failure = claim(os.Args[2:])
		case "status":
			result, failure = status(os.Args[2:])
		case "renew":
			result, failure = renew(os.Args[2:])
		case "release":
			result, failure = release(os.Args[2:])
		case "connect":
			foreground = true
			failure = runConnect(os.Args[2:])
		case "env":
			result, failure = envCommand(os.Args[2:])
		case "disconnect":
			result, failure = disconnectCommand(os.Args[2:])
		default:
			failure = &commandFailure{Code: "INVALID_ARGUMENT", Message: "支持命令：version、capabilities、claim、status、renew、release、connect、env、disconnect", exitCode: 2}
		}
	}
	if failure == nil && !foreground {
		if err := json.NewEncoder(os.Stdout).Encode(result); err == nil {
			return
		} else {
			failure = &commandFailure{Code: "OUTPUT_FAILED", Message: err.Error(), exitCode: 1}
		}
	}
	if failure == nil {
		return
	}
	if err := json.NewEncoder(os.Stderr).Encode(failure); err != nil {
		os.Exit(1)
	}
	os.Exit(failure.exitCode)
}
