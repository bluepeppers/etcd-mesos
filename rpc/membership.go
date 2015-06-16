/**
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package rpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/mesosphere/etcd-mesos/common"

	log "github.com/golang/glog"
)

type ClusterMemberList struct {
	Members []struct {
		Id         string   `json:"id"`
		Name       string   `json:"name"`
		PeerURLS   []string `json:"peerURLS"`
		ClientURLS []string `json:"clientURLS"`
	} `json:"members"`
}

func ConfigureInstance(
	running map[string]*common.EtcdConfig,
	newInstance *common.EtcdConfig,
) error {
	if len(running) == 0 {
		log.Info("No running members to configure.  Skipping configuration.")
		return nil
	}
	// TODO(tyler) enforce invariant that all existing nodes must be healthy before adding a new one!
	err := HealthCheck(running)
	if err != nil {
		log.Errorf("!!!! cluster failed health check: %+v", err)
		return err
	}

	backoff := 1
	log.Infof("trying to reconfigure cluster for newInstance %+v", newInstance)
	for retries := 0; retries < 5; retries++ {
		for _, args := range running {
			url := fmt.Sprintf(
				"http://%s:%d/v2/members",
				args.Host,
				args.ClientPort)
			data := fmt.Sprintf(
				`{"peerURLs": ["http://%s:%d"]}`,
				newInstance.Host,
				newInstance.RpcPort)

			req, err := http.NewRequest("POST", url, bytes.NewBuffer([]byte(data)))
			req.Header.Set("Content-Type", "application/json")

			client := &http.Client{
				Timeout: time.Second * 5,
			}
			resp, err := client.Do(req)
			if err != nil {
				log.Error(err)
				continue
			}
			defer resp.Body.Close()

			body, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				log.Errorf("Problem configuring instance: %s", err)
				continue
			}
			var memberList ClusterMemberList
			err = json.Unmarshal(body, &memberList)
			if err != nil {
				log.Errorf("Received unexpected response: %s", string(body))
				log.Errorf("Failed to unmarshal json: %s", err)
				continue
			}
			log.Infof("Successfully configured new node: %+v\n", memberList)
			return nil

			// TODO(tyler) invariant: member list should now contain node
		}
		log.Warningf("Failed to configure cluster for new instance.  "+
			"Backing off for %d seconds and retrying.", backoff)
		time.Sleep(time.Duration(backoff) * time.Second)
		backoff = backoff << 1
	}
	return errors.New("Failed to configure cluster: no nodes reachable.")
}

func MemberList(
	running map[string]*common.EtcdConfig,
) (nameToIdent map[string]string, err error) {
	nameToIdent = map[string]string{}

	if len(running) == 0 {
		log.Infoln("Skipping member query - none running or known.")
		return
	}

	backoff := 1
	for retries := 0; retries < 5; retries++ {
		for _, args := range running {
			url := fmt.Sprintf(
				"http://%s:%d/v2/members",
				args.Host,
				args.ClientPort)

			client := &http.Client{
				Timeout: time.Second * 5,
			}
			resp, err := client.Get(url)
			if err != nil {
				log.Error("Could not query %s for member list: %+v", args.Host, err)
				continue
			}
			defer resp.Body.Close()

			body, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				log.Error("could not query %s for member list", args.Host)
				continue
			}
			log.Info("MemberList response:", string(body))
			var memberList ClusterMemberList
			err = json.Unmarshal(body, &memberList)
			if err != nil {
				log.Error(err)
				continue
			}
			if len(memberList.Members) == 0 {
				err = errors.New("Remote node returned an empty etcd member list.")
				continue
			}
			log.Infof("got member list: %+v\n", memberList)

			for _, m := range memberList.Members {
				nameToIdent[m.Name] = m.Id
			}
			return nameToIdent, nil
		}
		log.Warningf("Failed to retrieve list of configured members.  "+
			"Backing off for %d seconds and retrying.", backoff)
		time.Sleep(time.Duration(backoff) * time.Second)
		backoff = backoff << 1
	}
	return nameToIdent, err
}

func RemoveInstance(running map[string]*common.EtcdConfig, task string) {
	log.Infof("Attempting to remove task %s from "+
		"the etcd cluster configuration.", task)
	members, err := MemberList(running)
	if err != nil {
		// TODO(tyler) handle
	}
	ident := members[task]
	backoff := 1
	for retries := 0; retries < 5; retries++ {
		for id, args := range running {
			if id == task {
				continue
			}
			url := fmt.Sprintf(
				"http://%s:%d/v2/members/%s",
				args.Host,
				args.ClientPort,
				ident)

			req, err := http.NewRequest("DELETE", url, nil)
			if err != nil {
				log.Error(err)
				continue
			}

			client := &http.Client{
				Timeout: time.Second * 5,
			}
			resp, err := client.Do(req)
			if err != nil {
				log.Error(err)
				continue
			}
			defer resp.Body.Close()

			body, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				log.Errorf("Problem removing instance for this attempt: %s", err)
				continue
			}
			log.Info("RemoveInstance response: ", string(body))
			if string(body) == "Method Not Allowed" {
				log.Error("Received error response while trying to remove " +
					"node from cluster configuration.")
				continue
			}
			var removeResponse struct {
				Message string `json="message"`
			}
			err = json.Unmarshal(body, &removeResponse)
			// TODO(tyler) invariant: member list should no longer contain node
			if err != nil {
				log.Errorf("Received unexpected response: %s", string(body))
				log.Errorf("Failed to unmarshal json: %s", err)
				continue
			}
			if strings.HasPrefix(removeResponse.Message, "Member permanently removed") {
				log.Info("Successfully removed member from cluster configuration.")
				return
			}
		}
		log.Warningf("Failed to retrieve list of configured members.  "+
			"Backing off for %d seconds and retrying.", backoff)
		time.Sleep(time.Duration(backoff) * time.Second)
		backoff = backoff << 1
	}
}
