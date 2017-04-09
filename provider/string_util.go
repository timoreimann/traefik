package provider

import "strings"

func splitAndTrimString(separatedString string) []string {
	listOfStrings := strings.Split(separatedString, ",")
	var trimmedListOfStrings []string
	for _, s := range listOfStrings {
		s = strings.TrimSpace(s)
		if len(s) > 0 {
			trimmedListOfStrings = append(trimmedListOfStrings, s)
		}
	}

	return trimmedListOfStrings
}
