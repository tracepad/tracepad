package store

import "fmt"

// ProvisionSpec is one declaratively provisioned project (spec 001 #9).
type ProvisionSpec struct {
	Name      string
	PublicKey string
	SecretKey string
}

// BootstrapResult reports what Bootstrap did. Secrets are present only for
// projects created in this run (generated ones); the store never returns
// stored secrets because it does not have them.
type BootstrapResult struct {
	Created []struct {
		Project Project
		Keys    KeyPair
	}
}

// Bootstrap provisions projects idempotently. Declared projects that already
// exist are left untouched (keys are never rotated from env). With no specs
// and an empty database it creates project "default" with generated keys.
func (s *Store) Bootstrap(specs []ProvisionSpec) (*BootstrapResult, error) {
	res := &BootstrapResult{}

	if len(specs) == 0 {
		n, err := s.CountProjects()
		if err != nil {
			return nil, err
		}
		if n > 0 {
			return res, nil
		}
		keys, err := GenerateKeyPair()
		if err != nil {
			return nil, err
		}
		specs = []ProvisionSpec{{Name: "default", PublicKey: keys.PublicKey, SecretKey: keys.Secret}}
	}

	for _, spec := range specs {
		existing, err := s.ProjectByName(spec.Name)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			continue
		}
		p, err := s.CreateProject(spec.Name, KeyPair{PublicKey: spec.PublicKey, Secret: spec.SecretKey})
		if err != nil {
			return nil, fmt.Errorf("bootstrap: %w", err)
		}
		res.Created = append(res.Created, struct {
			Project Project
			Keys    KeyPair
		}{*p, KeyPair{PublicKey: spec.PublicKey, Secret: spec.SecretKey}})
	}
	return res, nil
}
