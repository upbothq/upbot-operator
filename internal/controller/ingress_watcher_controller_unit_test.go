/*
Copyright 2025.

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

package controller

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMonitoringDisabled(t *testing.T) {
	cases := map[string]bool{
		"false": true, "False": true, "FALSE": true, "0": true, "f": true, "disabled": true, "Disabled": true, " false ": true,
		"true": false, "1": false, "": false, "yes": false, "anything": false,
	}
	for value, want := range cases {
		ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"upbot.app/monitor": value}}}
		if got := monitoringDisabled(ingress); got != want {
			t.Errorf("upbot.app/monitor=%q: disabled = %v, want %v", value, got, want)
		}
	}

	if monitoringDisabled(&networkingv1.Ingress{}) {
		t.Error("an Ingress without the annotation is monitored")
	}
}

func TestTargetScheme(t *testing.T) {
	cases := []struct {
		name string
		host string
		tls  []networkingv1.IngressTLS
		want string
	}{
		{"no TLS", "api.example.com", nil, "http://api.example.com"},
		{"host listed", "api.example.com", []networkingv1.IngressTLS{{Hosts: []string{"api.example.com"}}}, "https://api.example.com"},
		{"other host only", "api.example.com", []networkingv1.IngressTLS{{Hosts: []string{"www.example.com"}}}, "http://api.example.com"},
		{"listed in a second entry", "api.example.com", []networkingv1.IngressTLS{{Hosts: []string{"www.example.com"}}, {Hosts: []string{"API.example.com"}}}, "https://api.example.com"},
		{"entry without hosts", "api.example.com", []networkingv1.IngressTLS{{SecretName: "default"}}, "https://api.example.com"},
		{"wildcard", "api.example.com", []networkingv1.IngressTLS{{Hosts: []string{"*.example.com"}}}, "https://api.example.com"},
		{"wildcard covers one label", "a.b.example.com", []networkingv1.IngressTLS{{Hosts: []string{"*.example.com"}}}, "http://a.b.example.com"},
		{"wildcard not the apex", "example.com", []networkingv1.IngressTLS{{Hosts: []string{"*.example.com"}}}, "http://example.com"},
	}

	r := &IngressWatcherReconciler{}
	for _, c := range cases {
		ingress := &networkingv1.Ingress{Spec: networkingv1.IngressSpec{
			TLS:   c.tls,
			Rules: []networkingv1.IngressRule{{Host: c.host}},
		}}
		got, err := r.getTargetFromIngress(ingress)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: target = %q, want %q", c.name, got, c.want)
		}
	}
}
