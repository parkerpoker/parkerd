package daemon

import (
	cfg "github.com/parkerpoker/parkerd/internal/config"
)

type Service struct {
	inner *ProxyDaemon
}

func New(profile string, runtimeConfig cfg.RuntimeConfig, mode string) (*Service, error) {
	inner, err := NewProxyDaemon(profile, runtimeConfig, mode)
	if err != nil {
		return nil, err
	}
	return &Service{inner: inner}, nil
}

func (service *Service) Start() error {
	return service.inner.Start()
}

func (service *Service) Stop() error {
	return service.inner.Stop()
}

func (service *Service) Wait() {
	service.inner.Wait()
}
