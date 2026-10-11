package main

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
)

var definitionName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,99}$`)

func definitionAction(action string, args []string) error {
	if len(args) != 1 || !definitionName.MatchString(args[0]) {
		return fmt.Errorf("usage: minicrond %s NAME", action)
	}
	name := url.PathEscape(args[0])
	if action == "delete" {
		return requestPrint("DELETE", "/api/v1/jobs/"+name, nil)
	}
	return postPrint("/api/v1/workers/"+name+"/"+action, nil)
}

func recentRuns(args []string) error {
	if len(args) < 1 || !definitionName.MatchString(args[0]) {
		return fmt.Errorf("usage: minicrond runs NAME [--limit N]")
	}
	limit := 50
	if len(args) != 1 {
		if len(args) != 3 || args[1] != "--limit" {
			return fmt.Errorf("usage: minicrond runs NAME [--limit N]")
		}
		var err error
		limit, err = strconv.Atoi(args[2])
		if err != nil || limit < 1 || limit > 1000 {
			return fmt.Errorf("limit must be between 1 and 1000")
		}
	}
	return getPrint("/api/v1/runs?job=" + url.QueryEscape(args[0]) + "&limit=" + strconv.Itoa(limit))
}
