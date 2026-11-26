//go:build !linux

package main

import (
	"context"
	"fmt"
)

func (nativeLinks) host(context.Context) ([]link, error) {
	return nil, fmt.Errorf("native rtnetlink requires Linux")
}

func (nativeLinks) namespace(context.Context, int) ([]link, error) {
	return nil, fmt.Errorf("native network namespaces require Linux")
}

func (nativeLinks) addressed(context.Context) (map[int]bool, error) {
	return nil, fmt.Errorf("native rtnetlink requires Linux")
}

func (nativeLinks) mutate(context.Context, int, *link, string, string, string) error {
	return fmt.Errorf("native rtnetlink requires Linux")
}
