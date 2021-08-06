package main

import (
	"fmt"
	"k8s.io/klog/v2"
)

func tokenFromHost(host string) (string, error) {
	idToken, err := identityToken("https://" + host)
	klog.V(5).Infof("[authorization] receive idToken=%s", idToken)
	if err != nil {
		return "", fmt.Errorf("failed to fetch metadata token from host %s: %v", host, err)
	}

	return idToken, nil
}