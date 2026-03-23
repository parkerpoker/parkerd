package daemon

import (
	parker "github.com/danieldresner/arkade_fun/internal"
	cfg "github.com/danieldresner/arkade_fun/internal/config"
)

type Service struct {
	inner *parker.ProxyDaemon
}

func New(profile string, runtimeConfig cfg.RuntimeConfig, mode string) (*Service, error) {
	inner, err := parker.NewProxyDaemon(profile, runtimeConfig, mode)
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
