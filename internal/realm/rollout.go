package realm

import ("fmt";"sort";"time")

// FleetRevision pins the complete desired spec for old Units and rollback.
type FleetRevision struct {
 Generation uint64 `json:"generation"`
 Template UnitTemplate `json:"template"`
 Instances int `json:"instances"`
 MinimumAvailable int `json:"minimum_available"`
 MaxSurge int `json:"max_surge"`
 RequiredLabels map[string]string `json:"required_labels,omitempty"`
 SpreadLabel string `json:"spread_label,omitempty"`
}
func revisionOf(f Fleet) FleetRevision {return FleetRevision{f.Generation,f.Template,f.Instances,f.MinimumAvailable,f.MaxSurge,f.RequiredLabels,f.SpreadLabel}}
func (f Fleet) ForGeneration(g uint64)(Fleet,error) {
 if g==f.Generation {return f,nil}
 for _,r:=range f.History {if r.Generation==g {f.Template=r.Template;f.Generation=r.Generation;f.RequiredLabels=r.RequiredLabels;f.SpreadLabel=r.SpreadLabel;return f,nil}}
 return Fleet{},fmt.Errorf("Fleet %s revision %d is unavailable",f.Name,g)
}
func(s *Store) RollbackFleet(name string,g uint64)(Fleet,error) {
 s.Orchestration.Lock();defer s.Orchestration.Unlock()
 f,ok:=s.GetFleet(name);if !ok{return Fleet{},fmt.Errorf("unknown Fleet %s",name)}
 if len(f.History)==0{return Fleet{},fmt.Errorf("Fleet has no prior revision")}
 if g==0 {g=f.History[len(f.History)-1].Generation}
 var selected *FleetRevision
 for i:=range f.History {if f.History[i].Generation==g {selected=&f.History[i];break}}
 if selected==nil{return Fleet{},fmt.Errorf("revision %d not found",g)}
 r:=*selected;f.Template=r.Template;f.Instances=r.Instances;f.MinimumAvailable=r.MinimumAvailable;f.MaxSurge=r.MaxSurge;f.RequiredLabels=r.RequiredLabels;f.SpreadLabel=r.SpreadLabel
 if err:=s.putFleet(f);err!=nil{return Fleet{},err}
 f,_=s.GetFleet(name);return f,nil
}
type RolloutPlan struct {
 Keep []Assignment `json:"keep"`
 Create []Assignment `json:"create"`
 Retire []Assignment `json:"retire"`
 Available int `json:"available"`
 Updated int `json:"updated"`
 Complete bool `json:"complete"`
 Blocked string `json:"blocked,omitempty"`
}
func assignmentAvailable(state State,a Assignment,now time.Time)bool {
 n,ok:=state.Nodes[a.NodeID]
 return ok && n.State==NodeReady && a.State==AssignmentActive && !a.LeaseExpiresAt.IsZero() && now.Before(a.LeaseExpiresAt)
}
// Creation capacity never includes retirements not yet acknowledged by a node.
func(p *PlacementEngine) Rolling(state State,f Fleet,now time.Time)RolloutPlan {
 plan:=RolloutPlan{Keep:[]Assignment{},Create:[]Assignment{},Retire:[]Assignment{}}
 wanted:=map[string]bool{}
 for slot:=1;slot<=f.Instances;slot++ {wanted[fmt.Sprintf("%s-%03d-g%d",f.Name,slot,f.Generation)]=true}
 current:=[]Assignment{}
 for _,a:=range state.Assignments {if a.Fleet==f.Name {current=append(current,a);if assignmentAvailable(state,a,now){plan.Available++}}}
 sort.Slice(current,func(i,j int)bool{ai,aj:=assignmentAvailable(state,current[i],now),assignmentAvailable(state,current[j],now);if ai!=aj{return !ai};return current[i].ID<current[j].ID})
 available:=plan.Available
 for _,a:=range current {
  n,ok:=state.Nodes[a.NodeID]
  valid:=a.State!=AssignmentStopped && wanted[a.ID] && a.Generation==f.Generation && ok && eligible(n,f)
  if valid {plan.Keep=append(plan.Keep,a);delete(wanted,a.ID);plan.Updated++;continue}
  if assignmentAvailable(state,a,now) && available-1<f.MinimumAvailable {plan.Keep=append(plan.Keep,a);continue}
  plan.Retire=append(plan.Retire,a);if assignmentAvailable(state,a,now){available--}
 }
 surge:=f.MaxSurge;if surge==0{surge=1}
 capacity:=f.Instances+surge-len(current)
 ids:=[]string{}
 for id:=range wanted {occupied:=false;for _,a:=range current {if a.ID==id{occupied=true}};if !occupied{ids=append(ids,id)}}
 sort.Strings(ids);working:=cloneState(state);rankedCurrent:=append([]Assignment(nil),current...)
 for _,id:=range ids {
  if capacity<=0{plan.Blocked="waiting for readiness or acknowledged retirement";break}
  shared:=false
  for _,a:=range current {
   old,err:=f.ForGeneration(a.Generation);if err!=nil{shared=true;break}
   for _,m:=range f.Template.Mounts {for _,prev:=range old.Template.Mounts {if m.Disk==prev.Disk && (!m.ReadOnly || !prev.ReadOnly){shared=true}}}
  }
  if shared{plan.Blocked="shared writable Disk requires acknowledged retirement; adjust minimum_available or use independent Disks";break}
  ranked:=p.Rank(working,f,rankedCurrent)
  if len(ranked)==0{plan.Blocked="no eligible Node has capacity for a replacement";break}
  a:=Assignment{ID:id,Fleet:f.Name,NodeID:ranked[0].NodeID,State:AssignmentPlanned,Generation:f.Generation,CreatedAt:now,UpdatedAt:now}
  plan.Create=append(plan.Create,a);rankedCurrent=append(rankedCurrent,a);capacity--
  node:=working.Nodes[a.NodeID];node.Resources.MemoryUsedBytes+=f.Template.MemoryBytes;node.Resources.CPUMilliUsed+=int64(f.Template.CPUPercent*10);working.Nodes[a.NodeID]=node
 }
 plan.Complete=len(current)==f.Instances && plan.Updated==f.Instances && plan.Available==f.Instances && len(plan.Retire)==0 && len(plan.Create)==0
 if !plan.Complete && plan.Blocked=="" && len(plan.Create)==0 && len(plan.Retire)==0{plan.Blocked="waiting for target generation readiness"}
 return plan
}
