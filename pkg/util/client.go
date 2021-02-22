/*
Copyright 2021 The Kruise Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package util

import (
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

func NewClientFromManager(mgr manager.Manager, name string) client.Client {
	cfg := *mgr.GetConfig()
	cfg.UserAgent = fmt.Sprintf("kruise-manager/%s", name)

	c, err := client.New(&cfg, client.Options{Scheme: mgr.GetScheme(), Mapper: mgr.GetRESTMapper()})
	if err != nil {
		panic(err)
	}

	cache := mgr.GetCache()
	return &client.DelegatingClient{
		Reader: &client.DelegatingReader{
			CacheReader:  cache,
			ClientReader: c,
		},
		Writer:       c,
		StatusClient: c,
	}
}
