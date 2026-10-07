//go:build linux
package reconcile
import(
 "context";"os";"path/filepath";"testing";"time"
 "github.com/antonismor/Titanus-Core/internal/lease"
 "github.com/antonismor/Titanus-Core/internal/model"
 "github.com/antonismor/Titanus-Core/internal/realm"
 "github.com/antonismor/Titanus-Core/internal/source"
 "github.com/antonismor/Titanus-Core/internal/unitruntime"
)
type nativeRollout struct{m *unitruntime.Manager;leases *lease.Manager;t *testing.T;minimum int}
func(n *nativeRollout) EnsureUnit(_ string,s unitruntime.Spec)(unitruntime.State,error){return n.m.Ensure(s)}
func(n *nativeRollout) StartUnit(_,id string)(unitruntime.State,error){return n.m.EnsureRunning(id)}
func(n *nativeRollout) StopUnit(_,id string)(unitruntime.State,error){
 state,err:=n.m.Stop(id,5*time.Second);if err!=nil{return state,err}
 states,err:=n.m.List();if err!=nil{n.t.Fatal(err)}
 count:=0;for _,s:=range states{if s.Status==unitruntime.StatusActive && s.Ready{count++}}
 if count<n.minimum{n.t.Fatalf("native minimum availability violated: %d < %d",count,n.minimum)}
 return state,nil
}
func(n *nativeRollout) DeleteUnit(_,id string)error{return n.m.Delete(id)}
func(*nativeRollout) EnsureSource(string,string,*source.Manager)error{return nil}
func(n *nativeRollout) RenewLease(_,id,token string,ttl time.Duration)(lease.Record,error){return n.leases.Renew(id,token,ttl)}
func(n *nativeRollout) RevokeLease(_,id,token string)error{return n.leases.Revoke(id,token)}
func TestNativeRollingRuntime(t *testing.T){
 if os.Getenv("TITANUS_NATIVE_ROLLOUT_TEST")!="1"{t.Skip("requires root, cgroup v2, native init and prepared Source")}
 root:=t.TempDir();sources:=source.NewManager(root)
 if err:=sources.ImportDirectory("app","/tmp/titanus-rootfs");err!=nil{t.Fatal(err)}
 cfg:=unitruntime.Config{StateRoot:root,CgroupRoot:filepath.Join("/sys/fs/cgroup","titanus-rollout"),InitBinary:os.Getenv("TITANUS_INIT_BINARY")}
 m:=unitruntime.NewManager(cfg);leases:=lease.NewManager(m);defer leases.Close()
 defer func(){states,_:=m.List();for _,s:=range states{m.Stop(s.ID,time.Second);m.Delete(s.ID)}}()
 store,err:=realm.Open(root,"test");if err!=nil{t.Fatal(err)}
 if err:=store.UpsertNode(realm.Node{ID:"node",Address:"local",State:realm.NodeReady,Capabilities:[]model.Capability{model.CapabilityExecution},Resources:realm.Resources{MemoryBytes:4<<30,CPUMilliCapacity:4000}});err!=nil{t.Fatal(err)}
 fleet:=realm.Fleet{Name:"web",Instances:2,MinimumAvailable:2,MaxSurge:1,Template:realm.UnitTemplate{Source:"app",Command:[]string{"/bin/rollout-app","v1"},MemoryBytes:64<<20,CPUPercent:50,PidsMax:128,Health:unitruntime.Health{Readiness:&unitruntime.Probe{Protocol:"http",Port:8080,Path:"/ready",IntervalSeconds:1,FailureThreshold:1}}}}
 if err:=store.PutFleet(fleet);err!=nil{t.Fatal(err)}
 n:=&nativeRollout{m:m,leases:leases,t:t};c:=&Controller{Store:store,Nodes:n,Sources:sources}
 step:=func(){
  states,err:=m.List();if err!=nil{t.Fatal(err)};live:=0
  for _,s:=range states{if s.Status==unitruntime.StatusActive{live++};if err:=m.CheckHealth(context.Background(),s.ID);err!=nil{t.Fatal(err)}}
  if live>3{t.Fatalf("native surge exceeded: %d",live)}
  if err:=c.Once();err!=nil{t.Fatal(err)}
 }
 complete:=func(g uint64){
  deadline:=time.Now().Add(45*time.Second)
  for time.Now().Before(deadline){
   step();state:=c.Store.Snapshot();f:=state.Fleets["web"];p:=realm.NewPlacementEngine().Rolling(state,f,time.Now())
   if f.Generation==g && p.Complete{return};time.Sleep(150*time.Millisecond)
  }
  t.Fatalf("native rollout %d did not complete: %+v",g,c.Store.Snapshot().Assignments)
 }
 complete(1);n.minimum=2
 fleet.Template.Command=[]string{"/bin/rollout-app","v2"};if err:=store.PutFleet(fleet);err!=nil{t.Fatal(err)};complete(2)
 fleet.Template.Command=[]string{"/bin/rollout-app","broken"};if err:=store.PutFleet(fleet);err!=nil{t.Fatal(err)}
 for deadline:=time.Now().Add(4*time.Second);time.Now().Before(deadline);{step();time.Sleep(150*time.Millisecond)}
 old:=0;for _,a:=range store.Snapshot().Assignments{if a.Generation==2 && a.State==realm.AssignmentActive{old++}}
 if old!=2{t.Fatal("unhealthy native revision evicted old applications")}
 restored,err:=realm.Open(root,"test");if err!=nil{t.Fatal(err)};c.Store=restored
 if _,err:=restored.RollbackFleet("web",2);err!=nil{t.Fatal(err)};complete(4)
 t.Log("TITANUS_NATIVE_ROLLING_ROLLBACK_OK")
}
