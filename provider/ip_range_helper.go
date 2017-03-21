package provider

import (
	"net"
	"strings"
)

func makeIpNetFromCIDR(s string) net.IPNet {
	_, ipNet, err := net.ParseCIDR(s)
	if err != nil {
		return net.IPNet{}
	}
	return *ipNet;
}

func getIpSourceRangesFromAnnotation(annotation string) ([]net.IPNet, error) {
	annotation = strings.TrimSpace(annotation)

	if annotation == "" {
		return nil, nil
	}

	rangeStrings := strings.Split(annotation, ",")
	ipNets := []net.IPNet{}

	for _, rangeString := range rangeStrings {
		rangeString = strings.TrimSpace(rangeString)
		_, ipNet, err := net.ParseCIDR(rangeString)

		if err != nil {
			return []net.IPNet{}, err
		}

		ipNets = append(ipNets, *ipNet)
	}

	if len(ipNets) > 0 {
		return ipNets, nil
	}

	return []net.IPNet{}, nil
}
