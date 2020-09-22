// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package instance

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/tiup/pkg/repository/v0manifest"
	"github.com/pingcap/tiup/pkg/utils"
	"go.etcd.io/etcd/v3/pkg/proxy"
	"go.uber.org/zap"
)

func startProxyServer(addr string, targetAddr string) (proxy.Server, error) {
	url, err := url.Parse(addr)
	if err != nil {
		return nil, err
	}
	targetUrl, err := url.Parse(targetAddr)
	if err != nil {
		return nil, err
	}
	// TODO: it sinces tiup won't save or output log
	config := proxy.ServerConfig{
		Logger: zap.L(),
		From:   *url,
		To:     *targetUrl,
	}
	server := proxy.NewServer(config)
	select {
	case err := <-server.Error():
		return nil, err
	case <-time.After(time.Second):
		// wait for server to start
		return server, nil
	}
}

// TiKVInstance represent a running tikv-server
type TiKVInstance struct {
	instance
	pds []*PDInstance

	// Proxy related attributes
	AdvertisePort       int
	AdvertiseStatusPort int
	PortToProxyServer   map[int]proxy.Server

	Process
}

// NewTiKVInstance return a TiKVInstance
func NewTiKVInstance(binPath string, dir, host, configPath string, id int, pds []*PDInstance) *TiKVInstance {
	return &TiKVInstance{
		instance: instance{
			BinPath:    binPath,
			ID:         id,
			Dir:        dir,
			Host:       host,
			Port:       utils.MustGetFreePort(host, 20160),
			StatusPort: utils.MustGetFreePort(host, 20180),
			ConfigPath: configPath,
		},
		pds: pds,

		AdvertisePort:       utils.MustGetFreePort(host, 30160),
		AdvertiseStatusPort: utils.MustGetFreePort(host, 30180),
		PortToProxyServer:   make(map[int]proxy.Server),
	}
}

// Addr return the address of tikv.
func (inst *TiKVInstance) Addr() string {
	return fmt.Sprintf("%s:%d", inst.Host, inst.Port)
}

// startProxy starts proxy servers for Port and StatusPort
func (inst *TiKVInstance) startProxy() error {
	advertiseUrl := fmt.Sprintf("http://%s:%d", inst.Host, inst.AdvertisePort)
	url := fmt.Sprintf("http://%s:%d", inst.Host, inst.Port)
	proxyServer, err := startProxyServer(advertiseUrl, url)
	if err != nil {
		return err
	}
	inst.PortToProxyServer[inst.Port] = proxyServer

	advertiseStatusUrl := fmt.Sprintf("http://%s:%d", inst.Host, inst.AdvertiseStatusPort)
	statusUrl := fmt.Sprintf("http://%s:%d", inst.Host, inst.StatusPort)
	proxyServer, err = startProxyServer(advertiseStatusUrl, statusUrl)
	if err != nil {
		return err
	}
	inst.PortToProxyServer[inst.StatusPort] = proxyServer
	return nil
}

// Start calls set inst.cmd and Start
func (inst *TiKVInstance) Start(ctx context.Context, version v0manifest.Version) error {
	if err := os.MkdirAll(inst.Dir, 0755); err != nil {
		return err
	}
	if err := inst.checkConfig(); err != nil {
		return err
	}
	endpoints := make([]string, 0, len(inst.pds))
	for _, pd := range inst.pds {
		endpoints = append(endpoints, fmt.Sprintf("http://%s:%d", advertiseHost(inst.Host), pd.StatusPort))
	}
	args := []string{
		fmt.Sprintf("--addr=%s:%d", inst.Host, inst.Port),
		fmt.Sprintf("--advertise-addr=%s:%d", advertiseHost(inst.Host), inst.AdvertisePort),
		fmt.Sprintf("--status-addr=%s:%d", inst.Host, inst.StatusPort),
		fmt.Sprintf("--advertise-status-addr=%s:%d", advertiseHost(inst.Host), inst.AdvertiseStatusPort),
		fmt.Sprintf("--pd=%s", strings.Join(endpoints, ",")),
		fmt.Sprintf("--config=%s", inst.ConfigPath),
		fmt.Sprintf("--data-dir=%s", filepath.Join(inst.Dir, "data")),
		fmt.Sprintf("--log-file=%s", inst.LogFile()),
	}

	var err error
	if inst.Process, err = NewComponentProcess(ctx, inst.Dir, inst.BinPath, "tikv", version, args...); err != nil {
		return err
	}
	logIfErr(inst.Process.SetOutputFile(inst.LogFile()))

	err = inst.startProxy()
	if err != nil {
		return err
	}
	fmt.Printf("Start proxy for tikv %d:%d %d:%d\n", inst.AdvertisePort, inst.Port, inst.AdvertiseStatusPort, inst.StatusPort)
	return inst.Process.Start()
}

// Component return the component name.
func (inst *TiKVInstance) Component() string {
	return "tikv"
}

// LogFile return the log file name.
func (inst *TiKVInstance) LogFile() string {
	return filepath.Join(inst.Dir, "tikv.log")
}

// StoreAddr return the store address of TiKV
func (inst *TiKVInstance) StoreAddr() string {
	return fmt.Sprintf("%s:%d", advertiseHost(inst.Host), inst.Port)
}

func (inst *TiKVInstance) checkConfig() error {
	if inst.ConfigPath == "" {
		inst.ConfigPath = path.Join(inst.Dir, "tikv.toml")
	}

	_, err := os.Stat(inst.ConfigPath)
	if err == nil || os.IsExist(err) {
		return nil
	}
	if !os.IsNotExist(err) {
		return errors.Trace(err)
	}

	cf, err := os.Create(inst.ConfigPath)
	if err != nil {
		return errors.Trace(err)
	}

	defer cf.Close()
	if err := writeTiKVConfig(cf); err != nil {
		return errors.Trace(err)
	}

	return nil
}
