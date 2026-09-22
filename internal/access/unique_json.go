package access

import (
	"bytes"
	"io"
	"net/http"
	"strings"
"reflect"
"encoding/json"
"errors"

	"github.com/tyk-swe/olp/internal/oif"
)

// DecodeUnique is the bounded configuration boundary for model-significant
// documents. Validate original members before typed maps could turn ambiguous
// duplicate keys into last-writer-wins configuration. Source errors never echo
// values or names that might contain secrets.
func DecodeUnique(r *http.Request, destination any, limit int64) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return Fail(415,"unsupported_media_type","Send an application/json request.")
	}
	original:=r.Body
	defer original.Close()
	data,err:=io.ReadAll(io.LimitReader(original,limit+1))
	if err!=nil||int64(len(data))>limit{return Fail(400,"invalid_json","The request body exceeds this operation's bounded JSON contract.")}
	document,err:=oif.ParseJSON(data,oif.Limits{MaxBytes:int(limit),MaxDepth:64,MaxNodes:65536});if err!=nil{return Fail(400,"invalid_json","Configuration must contain one valid, unambiguous JSON document within its structural limits.")}
	if err:=canonicalMembers(document.Root(),reflect.TypeOf(destination),"");err!=nil{return Fail(400,"invalid_json","Configuration members must use their declared names and types; reset a default by removing it, not by replacing its definition with null.")}
	r.Body=io.NopCloser(bytes.NewReader(data))
	return Decode(r,destination)
}

// Go's JSON decoder accepts case-insensitive field aliases and scalar null as a
// zero value. Those coercions are unsafe for explicit model configuration. Raw
// native JSON remains opaque here, so distinct schema properties foo/Foo and
// native null retain their meaning instead of being conflated by reflection.
func canonicalMembers(value oif.Value, shape reflect.Type, fieldName string) error {
 if shape==nil||shape==reflect.TypeFor[json.RawMessage](){return nil}
 for shape.Kind()==reflect.Pointer{if value.Kind()==oif.Null{return nil};shape=shape.Elem()}
 if shape==reflect.TypeFor[json.RawMessage](){return nil}
 switch shape.Kind(){
 case reflect.Interface:return nil
 case reflect.Struct:
  if value.Kind()!=oif.Object{return errors.New("object required")}
  fields:=map[string]reflect.Type{}
  for i:=0;i<shape.NumField();i++{field:=shape.Field(i);tag:=strings.Split(field.Tag.Get("json"),",")[0];if field.PkgPath!=""||tag=="-"{continue};if tag==""{tag=field.Name};fields[tag]=field.Type}
  for _,member:=range value.Members(){field,ok:=fields[member.Name];if !ok{return errors.New("noncanonical field")};if err:=canonicalMembers(member.Value,field,member.Name);err!=nil{return err}}
 case reflect.Map:
  if value.Kind()==oif.Null{
   switch fieldName{case "semantic_headers","query_settings","operation_defaults","bindings","values","native_options":return errors.New("configuration definition cannot be null")};return nil
  }
  if value.Kind()!=oif.Object{return errors.New("object required")}
  for _,member:=range value.Members(){if err:=canonicalMembers(member.Value,shape.Elem(),member.Name);err!=nil{return err}}
 case reflect.Slice,reflect.Array:
  if value.Kind()==oif.Null{return nil}
  if value.Kind()!=oif.Array{return errors.New("array required")}
  for _,item:=range value.Elements(){if err:=canonicalMembers(item,shape.Elem(),fieldName);err!=nil{return err}}
 default:
  if value.Kind()==oif.Null{return errors.New("scalar cannot be null")}
 }
 return nil
}
