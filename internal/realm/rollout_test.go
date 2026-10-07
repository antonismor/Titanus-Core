package realm
import(
 "testing";"time"
 "github.com/antonismor/Titanus-Core/internal/disk"
 "github.com/antonismor/Titanus-Core/internal/model"
)
func TestRollingSharedWriterAndUnavailableLease(t *testing.T){
 now:=time.Now();f:=Fleet{Name:"db",Instances:1,MinimumAvailable:1,MaxSurge:1,Generation:2,Template:UnitTemplate{Source:"s",Command:[]string{"new"},Mounts:[]disk.Mount{{Disk:"data",Target:"/data"}}},History:[]FleetRevision{{Generation:1,Template:UnitTemplate{Mounts:[]disk.Mount{{Disk:"data",Target:"/data"}}}}}}
 state:=State{Nodes:map[string]Node{"n":{ID:"n",State:NodeReady,Capabilities:[]model.Capability{model.CapabilityExecution}}},Assignments:map[string]Assignment{"db-001-g1":{ID:"db-001-g1",Fleet:"db",NodeID:"n",Generation:1,State:AssignmentActive,LeaseExpiresAt:now.Add(time.Minute)}}}
 plan:=NewPlacementEngine().Rolling(state,f,now)
 if len(plan.Create)!=0 || len(plan.Retire)!=0 || plan.Blocked==""{t.Fatal("surge allowed simultaneous writable Disk access")}
 f.MinimumAvailable=0;plan=NewPlacementEngine().Rolling(state,f,now)
 if len(plan.Retire)!=1 || len(plan.Create)!=0{t.Fatal("replacement started before writer stop acknowledgement")}
 n:=state.Nodes["n"];n.State=NodeUnreachable;state.Nodes["n"]=n
 if p:=NewPlacementEngine().Rolling(state,f,now);p.Available!=0{t.Fatal("unreachable node counted available")}
}
func TestScalePreservesGenerationAndHistoryCannotBeInjected(t *testing.T){
 s,err:=Open(t.TempDir(),"test");if err!=nil{t.Fatal(err)}
 f:=Fleet{Name:"web",Instances:2,MinimumAvailable:2,Template:UnitTemplate{Source:"s",Command:[]string{"good"}},History:[]FleetRevision{{Generation:999}}}
 if err:=s.PutFleet(f);err!=nil{t.Fatal(err)};stored,_:=s.GetFleet("web");if len(stored.History)!=0{t.Fatal("untrusted history accepted")}
 scaled,err:=s.ScaleFleet("web",1);if err!=nil || scaled.Generation!=1 || scaled.MinimumAvailable!=1{t.Fatal("scale created revision or invalid minimum")}
 f.History=[]FleetRevision{{Generation:999}};f.Template.Command=[]string{"new"};if err:=s.PutFleet(f);err!=nil{t.Fatal(err)}
 stored,_=s.GetFleet("web");if len(stored.History)!=1 || stored.History[0].Generation!=1 || stored.History[0].Instances!=1{t.Fatal("history corrupted")}
 if _,err:=s.RollbackFleet("web",999);err==nil{t.Fatal("unknown rollback revision accepted")}
}
