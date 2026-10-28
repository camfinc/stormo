package sched

// SkillReferences includes disabled and paused jobs: an owner may resume them.
// A failed read prevents maintenance rather than silently removing protections.
func (s *Scheduler) SkillReferences() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	refs := []string{}
	for _, j := range f.Jobs {
		refs = append(refs, j.Skills()...)
	}
	return refs, nil
}
