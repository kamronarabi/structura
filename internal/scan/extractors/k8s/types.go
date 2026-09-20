package k8s

// The manifest structures below are deliberately hand-written rather than
// imported from k8s.io/api.
//
// The real API types pull in a very large module graph for what amounts to
// reading a dozen fields, and they are strict about versions in a way that
// works against us: a manifest written for a newer cluster than the vendored
// types must still be readable. Decoding is non-strict, so unknown fields are
// ignored and a manifest using a field we have never heard of still yields
// everything else it declares.

type typeMeta struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
}

type objectMeta struct {
	Name        string            `yaml:"name"`
	Namespace   string            `yaml:"namespace"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

// manifest is the shape every Kubernetes object shares, plus the union of
// the spec fields this extractor reads.
type manifest struct {
	typeMeta `yaml:",inline"`
	Metadata objectMeta `yaml:"metadata"`
	Spec     spec       `yaml:"spec"`
	// ConfigMap and Secret carry their payload at the top level. It is
	// typed loosely because real manifests put structured YAML under data
	// even though the API says the values are strings, and a strict type
	// here would fail the whole object over one nested value.
	Data map[string]any `yaml:"data"`
}

type spec struct {
	// Workload controllers
	Replicas    *int         `yaml:"replicas"`
	Selector    *selector    `yaml:"selector"`
	Template    *podTemplate `yaml:"template"`
	ServiceName string       `yaml:"serviceName"`
	JobTemplate *jobTemplate `yaml:"jobTemplate"`
	Schedule    string       `yaml:"schedule"`

	// Pod (a bare Pod puts its containers directly on spec)
	Containers     []container `yaml:"containers"`
	InitContainers []container `yaml:"initContainers"`

	// Service
	Type         string        `yaml:"type"`
	Ports        []servicePort `yaml:"ports"`
	ExternalName string        `yaml:"externalName"`
	ClusterIP    string        `yaml:"clusterIP"`

	// Ingress
	Rules            []ingressRule `yaml:"rules"`
	IngressClassName string        `yaml:"ingressClassName"`
	DefaultBackend   *backend      `yaml:"defaultBackend"`
	TLS              []ingressTLS  `yaml:"tls"`
}

// selector is a label selector. Only matchLabels is read: matchExpressions
// can express set membership and negation, which cannot be reduced to the
// equality join the resolver does, and guessing at it would produce edges
// that are not there.
type selector struct {
	MatchLabels map[string]string `yaml:"matchLabels"`
	// A Service's selector is a bare map rather than a LabelSelector.
	Inline map[string]string `yaml:",inline"`
}

// Labels returns the effective equality selector.
func (s *selector) Labels() map[string]string {
	if s == nil {
		return nil
	}
	if len(s.MatchLabels) > 0 {
		return s.MatchLabels
	}
	out := make(map[string]string, len(s.Inline))
	for k, v := range s.Inline {
		// matchExpressions decodes into Inline as a non-string value and is
		// dropped by the map[string]string typing; anything that survives is
		// a plain equality term.
		if k != "matchExpressions" && v != "" {
			out[k] = v
		}
	}
	return out
}

type podTemplate struct {
	Metadata objectMeta `yaml:"metadata"`
	Spec     podSpec    `yaml:"spec"`
}

type jobTemplate struct {
	Spec struct {
		Template *podTemplate `yaml:"template"`
	} `yaml:"spec"`
}

type podSpec struct {
	Containers         []container `yaml:"containers"`
	InitContainers     []container `yaml:"initContainers"`
	ServiceAccountName string      `yaml:"serviceAccountName"`
	Volumes            []volume    `yaml:"volumes"`
}

type container struct {
	Name    string          `yaml:"name"`
	Image   string          `yaml:"image"`
	Ports   []containerPort `yaml:"ports"`
	Env     []envVar        `yaml:"env"`
	EnvFrom []envFromSource `yaml:"envFrom"`
	Command []string        `yaml:"command"`
	Args    []string        `yaml:"args"`
}

type containerPort struct {
	Name          string `yaml:"name"`
	ContainerPort int    `yaml:"containerPort"`
	Protocol      string `yaml:"protocol"`
}

type envVar struct {
	Name      string        `yaml:"name"`
	Value     string        `yaml:"value"`
	ValueFrom *envVarSource `yaml:"valueFrom"`
}

type envVarSource struct {
	ConfigMapKeyRef *keyRef `yaml:"configMapKeyRef"`
	SecretKeyRef    *keyRef `yaml:"secretKeyRef"`
}

type keyRef struct {
	Name string `yaml:"name"`
	Key  string `yaml:"key"`
}

type envFromSource struct {
	ConfigMapRef *localRef `yaml:"configMapRef"`
	SecretRef    *localRef `yaml:"secretRef"`
	Prefix       string    `yaml:"prefix"`
}

type localRef struct {
	Name string `yaml:"name"`
}

type volume struct {
	Name                  string        `yaml:"name"`
	ConfigMap             *localRef     `yaml:"configMap"`
	Secret                *secretVolume `yaml:"secret"`
	PersistentVolumeClaim *pvcSource    `yaml:"persistentVolumeClaim"`
}

type secretVolume struct {
	SecretName string `yaml:"secretName"`
}

type pvcSource struct {
	ClaimName string `yaml:"claimName"`
}

type servicePort struct {
	Name       string `yaml:"name"`
	Port       int    `yaml:"port"`
	TargetPort any    `yaml:"targetPort"`
	Protocol   string `yaml:"protocol"`
}

type ingressRule struct {
	Host string `yaml:"host"`
	HTTP *struct {
		Paths []ingressPath `yaml:"paths"`
	} `yaml:"http"`
}

type ingressPath struct {
	Path    string  `yaml:"path"`
	Backend backend `yaml:"backend"`
}

type backend struct {
	Service *struct {
		Name string `yaml:"name"`
		Port struct {
			Number int    `yaml:"number"`
			Name   string `yaml:"name"`
		} `yaml:"port"`
	} `yaml:"service"`
	// networking.k8s.io/v1beta1 spelled the backend differently, and plenty
	// of committed manifests still use it.
	ServiceName string `yaml:"serviceName"`
	ServicePort any    `yaml:"servicePort"`
}

// Name returns the backend service name under either API version.
func (b backend) Name() string {
	if b.Service != nil && b.Service.Name != "" {
		return b.Service.Name
	}
	return b.ServiceName
}

type ingressTLS struct {
	Hosts      []string `yaml:"hosts"`
	SecretName string   `yaml:"secretName"`
}

// pods returns every pod template a manifest declares, across the controller
// kinds that nest them at different depths.
func (m *manifest) pods() []podSpec {
	switch {
	case m.Spec.Template != nil:
		return []podSpec{m.Spec.Template.Spec}
	case m.Spec.JobTemplate != nil && m.Spec.JobTemplate.Spec.Template != nil:
		return []podSpec{m.Spec.JobTemplate.Spec.Template.Spec}
	case len(m.Spec.Containers) > 0 || len(m.Spec.InitContainers) > 0:
		return []podSpec{{Containers: m.Spec.Containers, InitContainers: m.Spec.InitContainers}}
	default:
		return nil
	}
}

// podLabels returns the labels the controller stamps onto its pods, which is
// what a Service selector is matched against. It falls back to the object's
// own labels, which is what a bare Pod has.
func (m *manifest) podLabels() map[string]string {
	if m.Spec.Template != nil && len(m.Spec.Template.Metadata.Labels) > 0 {
		return m.Spec.Template.Metadata.Labels
	}
	if m.Spec.JobTemplate != nil && m.Spec.JobTemplate.Spec.Template != nil {
		if l := m.Spec.JobTemplate.Spec.Template.Metadata.Labels; len(l) > 0 {
			return l
		}
	}
	if labels := m.Spec.Selector.Labels(); len(labels) > 0 {
		return labels
	}
	return m.Metadata.Labels
}

// allContainers flattens init and main containers.
func (p podSpec) allContainers() []container {
	out := make([]container, 0, len(p.InitContainers)+len(p.Containers))
	out = append(out, p.InitContainers...)
	return append(out, p.Containers...)
}

// stringData returns the data entries that are plain scalars, which are the
// only ones that could name another component.
func (m *manifest) stringData() map[string]string {
	out := make(map[string]string, len(m.Data))
	for k, v := range m.Data {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}
