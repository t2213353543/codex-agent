package bps

import (
    "encoding/json"
    "fmt"
    "reflect"
    "strings"
    "testing"
)

const reviewIdentityPrefix = "Client-declared action tool identities (JSON; tool_choice restrictions below still apply): "

type reviewIdentity struct {
    Type string `json:"type"`
    Namespace string `json:"namespace"`
    Name string `json:"name"`
}

func reviewPrepared(t *testing.T, tools any, choice any, structured bool) *Request {
    t.Helper()
    body:=map[string]any{"model":"fixture","input":[]any{},"tools":tools,"tool_choice":choice,"parallel_tool_calls":false}
    if structured { body["text"]=json.RawMessage(`{"format":{"type":"json_object"}}`) }
    encoded,_:=json.Marshal(body)
    prepared,err:=Prepare(encoded,Options{TaskID:"fixture-task",TurnID:"fixture-turn",StructuredOutput:true})
    if err!=nil {t.Fatal(err)}
    return prepared
}

func reviewConstraint(t *testing.T, prepared *Request) string {
    t.Helper()
    constraint:=prepared.Policy.developerConstraint(prepared.Tools)
    if prepared.Structured!=nil{constraint=prepared.Policy.structuredDeveloperConstraint(prepared.Tools,prepared.Structured)}
    var body struct { Input []struct { Role string; Content []struct { Type string; Text string } } }
    if json.Unmarshal(prepared.Body,&body)!=nil{t.Fatal("prepared body invalid")}
    found:=false
    for _,item:=range body.Input{if item.Role=="developer"{for _,content:=range item.Content{found=found||content.Text==constraint}}}
    if !found{t.Fatal("constraint helper did not reach actual prepared developer input")}
    return constraint
}

func reviewIdentities(t *testing.T, constraint string) []reviewIdentity {
    t.Helper()
    for _,line:=range strings.Split(constraint,"\n") {
        if strings.HasPrefix(line,reviewIdentityPrefix) {
            var values []reviewIdentity
            if err:=json.Unmarshal([]byte(strings.TrimPrefix(line,reviewIdentityPrefix)),&values);err!=nil{t.Fatal(err)}
            if values==nil {t.Fatal("identity list must be [] rather than null")}
            return values
        }
    }
    t.Fatal("exact JSON client-tool identity list missing")
    return nil
}

func TestReviewAllowlistStableExactIdentityJSON(t *testing.T) {
    tools:=[]any{
        map[string]any{"type":"function","namespace":"z","name":"run_officejs"},
        map[string]any{"type":"custom","namespace":"a","name":"inspect"},
        map[string]any{"type":"function","namespace":"a","name":"run_officejs"},
        map[string]any{"type":"function","name":"list_skills"},
        map[string]any{"type":"function","namespace":"quoted\"scope","name":"line\nname"},
    }
    first:=reviewPrepared(t,tools,"auto",false)
    before:=append([]ToolIdentity(nil),first.Tools.Declared...)
    got:=reviewIdentities(t,reviewConstraint(t,first))
    want:=[]reviewIdentity{{"custom","a","inspect"},{"function","","list_skills"},{"function","a","run_officejs"},{"function","quoted\"scope","line\nname"},{"function","z","run_officejs"}}
    if !reflect.DeepEqual(got,want){t.Fatalf("identity order/escaping lost: %+v",got)}
    if !reflect.DeepEqual(before,first.Tools.Declared){t.Fatal("constraint sorting mutated tool catalog")}
    for i,j:=0,len(tools)-1;i<j;i,j=i+1,j-1{tools[i],tools[j]=tools[j],tools[i]}
    second:=reviewPrepared(t,tools,"auto",false)
    if reviewConstraint(t,first)!=reviewConstraint(t,second){t.Fatal("equivalent catalog order changed constraint")}
}

func TestReviewAllowlistEmptyStructuredAndPolicyRules(t *testing.T) {
    for _,structured:=range []bool{false,true}{
        for _,choice:=range []string{"auto","none"}{
            t.Run(fmt.Sprintf("empty/%t/%s",structured,choice),func(t *testing.T){
                prepared:=reviewPrepared(t,[]any{},choice,structured)
                constraint:=reviewConstraint(t,prepared)
                if len(reviewIdentities(t,constraint))!=0||!strings.Contains(constraint,"No client"){t.Fatal("empty client action boundary missing")}
                if choice=="none"&&!strings.Contains(constraint,"tool_choice=none"){t.Fatal("none restriction lost")}
                if structured && (!strings.Contains(constraint,"not a client action")||len(prepared.Tools.Declared)!=0){t.Fatal("formatter became client action")}
            })
        }
        for _,choice:=range []any{"auto","none","required",map[string]any{"type":"function","namespace":"customer","name":"run_officejs"}}{
            prepared:=reviewPrepared(t,[]any{map[string]any{"type":"function","namespace":"customer","name":"run_officejs"}},choice,structured)
            constraint:=reviewConstraint(t,prepared)
            if len(reviewIdentities(t,constraint))!=1||!strings.Contains(constraint,"at most one client"){t.Fatal("action identity/parallel restriction lost")}
            if prepared.Structured!=nil{
                for _,id:=range reviewIdentities(t,constraint){if id.Name==prepared.Structured.ToolName{t.Fatal("internal formatter in client allowlist")}}
                if !strings.Contains(constraint,prepared.Structured.ToolName){t.Fatal("formatter instructions disappeared")}
            }
            switch choice.(type){case string:
                if choice!="auto"&&!strings.Contains(constraint,"tool_choice="+choice.(string)){t.Fatal("choice rule disappeared")}
            default:if !strings.Contains(constraint,"selected client"){t.Fatal("forced exact selection disappeared")}}
        }
    }
}

func TestReviewAllowlistLargeCatalogNotTruncated(t *testing.T) {
    tools:=make([]any,1600)
    for i:=range tools{tools[i]=map[string]any{"type":"function","namespace":"customer","name":fmt.Sprintf("tool_%04d",i)}}
    prepared:=reviewPrepared(t,tools,"auto",false)
    got:=reviewIdentities(t,reviewConstraint(t,prepared))
    if len(got)!=1600||got[0].Name!="tool_0000"||got[1599].Name!="tool_1599"{t.Fatal("catalog clipped or hashed instead of exact identities")}
}

func TestReviewAllowlistDoesNotExpandNormalizer(t *testing.T) {
    for _,structured:=range []bool{false,true}{
        for _,choice:=range []string{"auto","none","required"}{
            prepared:=reviewPrepared(t,[]any{map[string]any{"type":"function","namespace":"customer","name":"run_officejs"},map[string]any{"type":"custom","namespace":"customer","name":"inspect"}},choice,structured)
            for _,test:=range []struct{Name,Namespace,Type string;Allowed bool}{
                {"run_officejs","customer","function_call",choice!="none"},
                {"run_officejs","other","function_call",false},
                {"run_officejs","customer","custom_tool_call",false},
                {"list_slides","","function_call",false},
                {"update_plan","","function_call",false},
                {"inspect","customer","custom_tool_call",choice!="none"},
            }{
                item:=map[string]json.RawMessage{"type":raw(test.Type),"name":raw(test.Name),"namespace":raw(test.Namespace),"id":raw("fc_fixture"),"call_id":raw("call_fixture"),"arguments":raw("{}"),"input":raw("fixture")}
                if (prepared.Tools.NormalizeItem(item)==nil)!=test.Allowed{t.Fatalf("normalizer permissions changed: %+v choice=%s",test,choice)}
            }
            if prepared.Structured!=nil{
                internal:=map[string]json.RawMessage{"type":raw("function_call"),"name":raw(prepared.Structured.ToolName),"arguments":raw("{}"),"call_id":raw("formatter")}
                if prepared.Tools.NormalizeItem(internal)==nil{t.Fatal("internal formatter authorized as public tool")}
            }
        }
    }
}

func TestReviewAllowlistDeferredDiscoveryStaysSeparate(t *testing.T) {
    prepared:=reviewPrepared(t,[]any{map[string]any{"type":"tool_search","execution":"client"}},"auto",false)
    constraint:=reviewConstraint(t,prepared)
    if len(reviewIdentities(t,constraint))!=0||!strings.Contains(constraint,"tool_search")||!strings.Contains(constraint,"actually declared by tool_search_output"){t.Fatal("discovery authorization changed")}
}

func TestReviewAllowlistForcedAndParallelRemainEnforced(t *testing.T) {
    tools:=[]any{map[string]any{"type":"function","namespace":"customer","name":"run_officejs"},map[string]any{"type":"function","namespace":"other","name":"run_officejs"}}
    for _,structured:=range []bool{false,true}{
        forced:=map[string]any{"type":"function","namespace":"customer","name":"run_officejs"}
        prepared:=reviewPrepared(t,tools,forced,structured)
        selected:=raw(map[string]any{"type":"function_call","namespace":"customer","name":"run_officejs","call_id":"selected","arguments":"{}"})
        wrong:=raw(map[string]any{"type":"function_call","namespace":"other","name":"run_officejs","call_id":"other","arguments":"{}"})
        if prepared.Tools.ValidateResponse([]json.RawMessage{selected},prepared.Policy)!=nil{t.Fatal("declared selected Office-named action rejected")}
        if prepared.Tools.ValidateResponse([]json.RawMessage{wrong},prepared.Policy)==nil{t.Fatal("wrong exact identity escaped forced policy")}
        if prepared.Tools.ValidateResponse(nil,prepared.Policy)==nil{t.Fatal("forced action requirement disappeared")}
        second:=raw(map[string]any{"type":"function_call","namespace":"customer","name":"run_officejs","call_id":"second","arguments":"{}"})
        if prepared.Tools.ValidateResponse([]json.RawMessage{selected,second},prepared.Policy)==nil{t.Fatal("parallel=false escaped")}
    }
}
