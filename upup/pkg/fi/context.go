/*
Copyright 2016 The Kubernetes Authors.

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

package fi

import (
	"bytes"
	"fmt"
	"github.com/golang/glog"
	"io/ioutil"
	"k8s.io/kops/util/pkg/vfs"
	"k8s.io/kubernetes/federation/pkg/dnsprovider"
	"os"
	"reflect"
	"strings"
	"time"
)

type Context struct {
	Tmpdir string

	Target            Target
	DNS               dnsprovider.Interface
	Cloud             Cloud
	Keystore          Keystore
	SecretStore       SecretStore
	ClusterConfigBase vfs.Path

	CheckExisting bool

	tasks map[string]Task
}

func NewContext(target Target, cloud Cloud, keystore Keystore, secretStore SecretStore, clusterConfigBase vfs.Path, checkExisting bool, tasks map[string]Task) (*Context, error) {
	c := &Context{
		Cloud:             cloud,
		Target:            target,
		Keystore:          keystore,
		SecretStore:       secretStore,
		ClusterConfigBase: clusterConfigBase,
		CheckExisting:     checkExisting,
		tasks:             tasks,
	}

	t, err := ioutil.TempDir("", "deploy")
	if err != nil {
		return nil, fmt.Errorf("error creating temporary directory: %v", err)
	}
	c.Tmpdir = t

	return c, nil
}

func (c *Context) AllTasks() map[string]Task {
	return c.tasks
}

func (c *Context) RunTasks(maxTaskDuration time.Duration) error {
	e := &executor{
		context: c,
	}
	return e.RunTasks(c.tasks, maxTaskDuration)
}

func (c *Context) Close() {
	glog.V(2).Infof("deleting temp dir: %q", c.Tmpdir)
	if c.Tmpdir != "" {
		err := os.RemoveAll(c.Tmpdir)
		if err != nil {
			glog.Warningf("unable to delete temporary directory %q: %v", c.Tmpdir, err)
		}
	}
}

//func (c *Context) MergeOptions(options Options) error {
//	return c.Options.Merge(options)
//}

func (c *Context) NewTempDir(prefix string) (string, error) {
	t, err := ioutil.TempDir(c.Tmpdir, prefix)
	if err != nil {
		return "", fmt.Errorf("error creating temporary directory: %v", err)
	}
	return t, nil
}

var typeContextPtr = reflect.TypeOf((*Context)(nil))

func (c *Context) Render(a, e, changes Task) error {
	var lifecycle *Lifecycle
	if hl, ok := e.(HasLifecycle); ok {
		lifecycle = hl.GetLifecycle()
	}

	if lifecycle != nil {
		if reflect.ValueOf(a).IsNil() {

			switch *lifecycle {
			case LifecycleExistsAndValidates:
				return fmt.Errorf("Lifecycle set to ExistsAndValidates, but object was not found")
			case LifecycleExistsAndWarnIfChanges:
				return fmt.Errorf("Lifecycle set to ExistsAndWarnIfChanges, but object was not found")
			}
		} else {
			switch *lifecycle {
			case LifecycleExistsAndValidates, LifecycleExistsAndWarnIfChanges:
				out := os.Stderr
				changeList, err := buildChangeList(a, e, changes)
				if err != nil {
					return err
				}

				b := &bytes.Buffer{}
				taskName := getTaskName(e)
				fmt.Fprintf(b, "Object from different phase did not match, problems possible:\n")
				fmt.Fprintf(b, "  %s/%s\n", taskName, "?")
				for _, change := range changeList {
					lines := strings.Split(change.Description, "\n")
					if len(lines) == 1 {
						fmt.Fprintf(b, "  \t%-20s\t%s\n", change.FieldName, change.Description)
					} else {
						fmt.Fprintf(b, "  \t%-20s\n", change.FieldName)
						for _, line := range lines {
							fmt.Fprintf(b, "  \t%-20s\t%s\n", "", line)
						}
					}
				}
				fmt.Fprintf(b, "\n")
				b.WriteTo(out)

				if *lifecycle == LifecycleExistsAndValidates {
					return fmt.Errorf("Lifecycle set to ExistsAndValidates, but object did not match")
				} else {
					// Warn, but then we continue
					return nil
				}
			}
		}
	}

	if _, ok := c.Target.(*DryRunTarget); ok {
		return c.Target.(*DryRunTarget).Render(a, e, changes)
	}

	v := reflect.ValueOf(e)
	vType := v.Type()

	targetType := reflect.ValueOf(c.Target).Type()

	var renderer *reflect.Method
	var rendererArgs []reflect.Value

	for i := 0; i < vType.NumMethod(); i++ {
		method := vType.Method(i)
		if !strings.HasPrefix(method.Name, "Render") {
			continue
		}
		match := true

		var args []reflect.Value
		for j := 0; j < method.Type.NumIn(); j++ {
			arg := method.Type.In(j)
			if arg.ConvertibleTo(vType) {
				continue
			}
			if arg.ConvertibleTo(typeContextPtr) {
				args = append(args, reflect.ValueOf(c))
				continue
			}
			if arg.ConvertibleTo(targetType) {
				args = append(args, reflect.ValueOf(c.Target))
				continue
			}
			match = false
			break
		}
		if match {
			if renderer != nil {
				return fmt.Errorf("Found multiple Render methods that could be invokved on %T", e)
			}
			renderer = &method
			rendererArgs = args
		}

	}
	if renderer == nil {
		return fmt.Errorf("Could not find Render method on type %T (target %T)", e, c.Target)
	}
	rendererArgs = append(rendererArgs, reflect.ValueOf(a))
	rendererArgs = append(rendererArgs, reflect.ValueOf(e))
	rendererArgs = append(rendererArgs, reflect.ValueOf(changes))
	glog.V(11).Infof("Calling method %s on %T", renderer.Name, e)
	m := v.MethodByName(renderer.Name)
	rv := m.Call(rendererArgs)
	var rvErr error
	if !rv[0].IsNil() {
		rvErr = rv[0].Interface().(error)
	}
	return rvErr
}
