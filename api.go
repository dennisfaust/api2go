package api2go

import (
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"github.com/golang/gddo/httputil"
	"github.com/julienschmidt/httprouter"
	"github.com/manyminds/api2go/jsonapi"
)

const defaultContentTypHeader = "application/vnd.api+json"

type response struct {
	Meta   map[string]interface{}
	Data   interface{}
	Status int
}

func (r response) Metadata() map[string]interface{} {
	return r.Meta
}

func (r response) Result() interface{} {
	return r.Data
}

func (r response) StatusCode() int {
	return r.Status
}

type information struct {
	prefix  string
	baseURL string
}

func (i information) GetBaseURL() string {
	return i.baseURL
}

func (i information) GetPrefix() string {
	return i.prefix
}

type paginationQueryParams struct {
	number, size, offset, limit string
}

func newPaginationQueryParams(r *http.Request) paginationQueryParams {
	var result paginationQueryParams

	queryParams := r.URL.Query()
	result.number = queryParams.Get("page[number]")
	result.size = queryParams.Get("page[size]")
	result.offset = queryParams.Get("page[offset]")
	result.limit = queryParams.Get("page[limit]")

	return result
}

func (p paginationQueryParams) isValid() bool {
	if p.number == "" && p.size == "" && p.offset == "" && p.limit == "" {
		return false
	}

	if p.number != "" && p.size != "" && p.offset == "" && p.limit == "" {
		return true
	}

	if p.number == "" && p.size == "" && p.offset != "" && p.limit != "" {
		return true
	}

	return false
}

func (p paginationQueryParams) getLinks(r *http.Request, count uint, info information) (result map[string]string, err error) {
	result = make(map[string]string)

	params := r.URL.Query()
	prefix := ""
	baseURL := info.GetBaseURL()
	if baseURL != "" {
		prefix = baseURL
	}
	requestURL := fmt.Sprintf("%s%s", prefix, r.URL.Path)

	if p.number != "" {
		// we have number & size params
		var number uint64
		number, err = strconv.ParseUint(p.number, 10, 64)
		if err != nil {
			return
		}

		if p.number != "1" {
			params.Set("page[number]", "1")
			query, _ := url.QueryUnescape(params.Encode())
			result["first"] = fmt.Sprintf("%s?%s", requestURL, query)

			params.Set("page[number]", strconv.FormatUint(number-1, 10))
			query, _ = url.QueryUnescape(params.Encode())
			result["prev"] = fmt.Sprintf("%s?%s", requestURL, query)
		}

		// calculate last page number
		var size uint64
		size, err = strconv.ParseUint(p.size, 10, 64)
		if err != nil {
			return
		}
		totalPages := (uint64(count) / size)
		if (uint64(count) % size) != 0 {
			// there is one more page with some len(items) < size
			totalPages++
		}

		if number != totalPages {
			params.Set("page[number]", strconv.FormatUint(number+1, 10))
			query, _ := url.QueryUnescape(params.Encode())
			result["next"] = fmt.Sprintf("%s?%s", requestURL, query)

			params.Set("page[number]", strconv.FormatUint(totalPages, 10))
			query, _ = url.QueryUnescape(params.Encode())
			result["last"] = fmt.Sprintf("%s?%s", requestURL, query)
		}
	} else {
		// we have offset & limit params
		var offset, limit uint64
		offset, err = strconv.ParseUint(p.offset, 10, 64)
		if err != nil {
			return
		}
		limit, err = strconv.ParseUint(p.limit, 10, 64)
		if err != nil {
			return
		}

		if p.offset != "0" {
			params.Set("page[offset]", "0")
			query, _ := url.QueryUnescape(params.Encode())
			result["first"] = fmt.Sprintf("%s?%s", requestURL, query)

			var prevOffset uint64
			if limit > offset {
				prevOffset = 0
			} else {
				prevOffset = offset - limit
			}
			params.Set("page[offset]", strconv.FormatUint(prevOffset, 10))
			query, _ = url.QueryUnescape(params.Encode())
			result["prev"] = fmt.Sprintf("%s?%s", requestURL, query)
		}

		// check if there are more entries to be loaded
		if (offset + limit) < uint64(count) {
			params.Set("page[offset]", strconv.FormatUint(offset+limit, 10))
			query, _ := url.QueryUnescape(params.Encode())
			result["next"] = fmt.Sprintf("%s?%s", requestURL, query)

			params.Set("page[offset]", strconv.FormatUint(uint64(count)-limit, 10))
			query, _ = url.QueryUnescape(params.Encode())
			result["last"] = fmt.Sprintf("%s?%s", requestURL, query)
		}
	}

	return
}

type notAllowedHandler struct {
	marshalers map[string]ContentMarshaler
}

func (n notAllowedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	err := NewHTTPError(nil, "Method Not Allowed", http.StatusMethodNotAllowed)
	w.WriteHeader(http.StatusMethodNotAllowed)
	handleError(err, w, r, n.marshalers)
}

type resource struct {
	resourceType reflect.Type
	source       CRUD
	name         string
	marshalers   map[string]ContentMarshaler
}

func (api *API) addResource(prototype jsonapi.MarshalIdentifier, source CRUD, marshalers map[string]ContentMarshaler) *resource {
	resourceType := reflect.TypeOf(prototype)
	if resourceType.Kind() != reflect.Struct && resourceType.Kind() != reflect.Ptr {
		panic("pass an empty resource struct or a struct pointer to AddResource!")
	}

	var ptrPrototype interface{}
	var name string

	if resourceType.Kind() == reflect.Struct {
		ptrPrototype = reflect.New(resourceType).Interface()
		name = resourceType.Name()
	} else {
		ptrPrototype = reflect.ValueOf(prototype).Interface()
		name = resourceType.Elem().Name()
	}

	// check if EntityNamer interface is implemented and use that as name
	entityName, ok := prototype.(jsonapi.EntityNamer)
	if ok {
		name = entityName.GetName()
	} else {
		name = jsonapi.Jsonify(jsonapi.Pluralize(name))
	}

	res := resource{
		resourceType: resourceType,
		name:         name,
		source:       source,
		marshalers:   marshalers,
	}

	api.router.Handle("OPTIONS", api.prefix+name, func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		w.Header().Set("Allow", "GET,POST,PATCH,OPTIONS")
		w.WriteHeader(http.StatusNoContent)
	})

	api.router.Handle("OPTIONS", api.prefix+name+"/:id", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		w.Header().Set("Allow", "GET,PATCH,DELETE,OPTIONS")
		w.WriteHeader(http.StatusNoContent)
	})

	api.router.GET(api.prefix+name, func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		err := res.handleIndex(w, r, api.info)
		if err != nil {
			handleError(err, w, r, marshalers)
		}
	})

	api.router.GET(api.prefix+name+"/:id", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		err := res.handleRead(w, r, ps, api.info)
		if err != nil {
			handleError(err, w, r, marshalers)
		}
	})

	// generate all routes for linked relations if there are relations
	casted, ok := prototype.(jsonapi.MarshalReferences)
	if ok {
		relations := casted.GetReferences()
		for _, relation := range relations {
			api.router.GET(api.prefix+name+"/:id/relationships/"+relation.Name, func(relation jsonapi.Reference) httprouter.Handle {
				return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
					err := res.handleReadRelation(w, r, ps, api.info, relation)
					if err != nil {
						handleError(err, w, r, marshalers)
					}
				}
			}(relation))

			api.router.GET(api.prefix+name+"/:id/"+relation.Name, func(relation jsonapi.Reference) httprouter.Handle {
				return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
					err := res.handleLinked(api, w, r, ps, relation, api.info)
					if err != nil {
						handleError(err, w, r, marshalers)
					}
				}
			}(relation))

			api.router.PATCH(api.prefix+name+"/:id/relationships/"+relation.Name, func(relation jsonapi.Reference) httprouter.Handle {
				return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
					err := res.handleReplaceRelation(w, r, ps, relation)
					if err != nil {
						handleError(err, w, r, marshalers)
					}
				}
			}(relation))

			if _, ok := ptrPrototype.(jsonapi.EditToManyRelations); ok && relation.Name == jsonapi.Pluralize(relation.Name) {
				// generate additional routes to manipulate to-many relationships
				api.router.POST(api.prefix+name+"/:id/relationships/"+relation.Name, func(relation jsonapi.Reference) httprouter.Handle {
					return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
						err := res.handleAddToManyRelation(w, r, ps, relation)
						if err != nil {
							handleError(err, w, r, marshalers)
						}
					}
				}(relation))

				api.router.DELETE(api.prefix+name+"/:id/relationships/"+relation.Name, func(relation jsonapi.Reference) httprouter.Handle {
					return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
						err := res.handleDeleteToManyRelation(w, r, ps, relation)
						if err != nil {
							handleError(err, w, r, marshalers)
						}
					}
				}(relation))
			}
		}
	}

	api.router.POST(api.prefix+name, func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		err := res.handleCreate(w, r, api.prefix, api.info)
		if err != nil {
			handleError(err, w, r, marshalers)
		}
	})

	api.router.DELETE(api.prefix+name+"/:id", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		err := res.handleDelete(w, r, ps)
		if err != nil {
			handleError(err, w, r, marshalers)
		}
	})

	api.router.PATCH(api.prefix+name+"/:id", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		err := res.handleUpdate(w, r, ps)
		if err != nil {
			handleError(err, w, r, marshalers)
		}
	})

	api.resources = append(api.resources, res)

	return &res
}

func buildRequest(r *http.Request) Request {
	req := Request{PlainRequest: r}
	params := make(map[string][]string)
	for key, values := range r.URL.Query() {
		params[key] = strings.Split(values[0], ",")
	}
	req.QueryParams = params
	req.Header = r.Header
	return req
}

func (res *resource) handleIndex(w http.ResponseWriter, r *http.Request, info information) error {
	pagination := newPaginationQueryParams(r)
	if pagination.isValid() {
		source, ok := res.source.(PaginatedFindAll)
		if !ok {
			return NewHTTPError(nil, "Resource does not implement the PaginatedFindAll interface", http.StatusNotFound)
		}

		count, response, err := source.PaginatedFindAll(buildRequest(r))
		if err != nil {
			return err
		}

		paginationLinks, err := pagination.getLinks(r, count, info)
		if err != nil {
			return err
		}

		return respondWithPagination(response, info, http.StatusOK, paginationLinks, w, r, res.marshalers)
	}
	source, ok := res.source.(FindAll)
	if !ok {
		return NewHTTPError(nil, "Resource does not implement the FindAll interface", http.StatusNotFound)
	}

	response, err := source.FindAll(buildRequest(r))
	if err != nil {
		return err
	}

	return respondWith(response, info, http.StatusOK, w, r, res.marshalers)
}

func (res *resource) handleRead(w http.ResponseWriter, r *http.Request, ps httprouter.Params, info information) error {
	id := ps.ByName("id")

	response, err := res.source.FindOne(id, buildRequest(r))

	if err != nil {
		return err
	}

	return respondWith(response, info, http.StatusOK, w, r, res.marshalers)
}

func (res *resource) handleReadRelation(w http.ResponseWriter, r *http.Request, ps httprouter.Params, info information, relation jsonapi.Reference) error {
	id := ps.ByName("id")

	obj, err := res.source.FindOne(id, buildRequest(r))
	if err != nil {
		return err
	}

	internalError := NewHTTPError(nil, "Internal server error, invalid object structure", http.StatusInternalServerError)

	marshalled, err := jsonapi.MarshalWithURLs(obj.Result(), info)
	data, ok := marshalled["data"]
	if !ok {
		return internalError
	}
	relationships, ok := data.(map[string]interface{})["relationships"]
	if !ok {
		return internalError
	}

	rel, ok := relationships.(map[string]map[string]interface{})[relation.Name]
	if !ok {
		return NewHTTPError(nil, fmt.Sprintf("There is no relation with the name %s", relation.Name), http.StatusNotFound)
	}
	links, ok := rel["links"].(map[string]string)
	if !ok {
		return internalError
	}
	self, ok := links["self"]
	if !ok {
		return internalError
	}
	related, ok := links["related"]
	if !ok {
		return internalError
	}
	relationData, ok := rel["data"]
	if !ok {
		return internalError
	}

	result := map[string]interface{}{}
	result["links"] = map[string]interface{}{
		"self":    self,
		"related": related,
	}
	result["data"] = relationData
	meta := obj.Metadata()
	if len(meta) > 0 {
		result["meta"] = meta
	}

	return marshalResponse(result, w, http.StatusOK, r, res.marshalers)
}

// try to find the referenced resource and call the findAll Method with referencing resource id as param
func (res *resource) handleLinked(api *API, w http.ResponseWriter, r *http.Request, ps httprouter.Params, linked jsonapi.Reference, info information) error {
	id := ps.ByName("id")
	for _, resource := range api.resources {
		if resource.name == linked.Type {
			request := buildRequest(r)
			request.QueryParams[res.name+"ID"] = []string{id}
			request.QueryParams[res.name+"Name"] = []string{linked.Name}

			// check for pagination, otherwise normal FindAll
			pagination := newPaginationQueryParams(r)
			if pagination.isValid() {
				source, ok := resource.source.(PaginatedFindAll)
				if !ok {
					return NewHTTPError(nil, "Resource does not implement the PaginatedFindAll interface", http.StatusNotFound)
				}

				var count uint
				count, response, err := source.PaginatedFindAll(request)
				if err != nil {
					return err
				}

				paginationLinks, err := pagination.getLinks(r, count, info)
				if err != nil {
					return err
				}

				return respondWithPagination(response, info, http.StatusOK, paginationLinks, w, r, res.marshalers)
			}

			source, ok := resource.source.(FindAll)
			if !ok {
				return NewHTTPError(nil, "Resource does not implement the FindAll interface", http.StatusNotFound)
			}

			obj, err := source.FindAll(request)
			if err != nil {
				return err
			}
			return respondWith(obj, info, http.StatusOK, w, r, res.marshalers)
		}
	}

	err := Error{
		Status: string(http.StatusNotFound),
		Title:  "Not Found",
		Detail: "No resource handler is registered to handle the linked resource " + linked.Name,
	}

	answ := response{Data: err, Status: http.StatusNotFound}

	return respondWith(answ, info, http.StatusNotFound, w, r, res.marshalers)

}

func (res *resource) handleCreate(w http.ResponseWriter, r *http.Request, prefix string, info information) error {
	ctx, err := unmarshalRequest(r, res.marshalers)
	if err != nil {
		return err
	}
	newObjs := reflect.MakeSlice(reflect.SliceOf(res.resourceType), 0, 0)

	structType := res.resourceType
	if structType.Kind() == reflect.Ptr {
		structType = structType.Elem()
	}

	err = jsonapi.UnmarshalInto(ctx, structType, &newObjs)
	if err != nil {
		return err
	}
	if newObjs.Len() != 1 {
		return errors.New("expected one object in POST")
	}

	//TODO create multiple objects not only one.
	newObj := newObjs.Index(0).Interface()

	response, err := res.source.Create(newObj, buildRequest(r))
	if err != nil {
		return err
	}

	result, ok := response.Result().(jsonapi.MarshalIdentifier)

	if !ok {
		return fmt.Errorf("Expected one newly created object by resource %s", res.name)
	}
	w.Header().Set("Location", prefix+res.name+"/"+result.GetID())

	// handle 200 status codes
	switch response.StatusCode() {
	case http.StatusCreated:
		return respondWith(response, info, http.StatusCreated, w, r, res.marshalers)
	case http.StatusNoContent:
		w.WriteHeader(response.StatusCode())
		return nil
	case http.StatusAccepted:
		w.WriteHeader(response.StatusCode())
		return nil
	default:
		return fmt.Errorf("invalid status code %d from resource %s for method Create", response.StatusCode(), res.name)
	}
}

func (res *resource) handleUpdate(w http.ResponseWriter, r *http.Request, ps httprouter.Params) error {
	obj, err := res.source.FindOne(ps.ByName("id"), buildRequest(r))
	if err != nil {
		return err
	}

	ctx, err := unmarshalRequest(r, res.marshalers)
	if err != nil {
		return err
	}

	data, ok := ctx["data"]

	if !ok {
		return NewHTTPError(
			errors.New("Forbidden"),
			"missing mandatory data key.",
			http.StatusForbidden,
		)
	}

	check, ok := data.(map[string]interface{})
	if !ok {
		return NewHTTPError(
			errors.New("Forbidden"),
			"data must contain an object.",
			http.StatusForbidden,
		)
	}

	if _, ok := check["id"]; !ok {
		return NewHTTPError(
			errors.New("Forbidden"),
			"missing mandatory id key.",
			http.StatusForbidden,
		)
	}

	if _, ok := check["type"]; !ok {
		return NewHTTPError(
			errors.New("Forbidden"),
			"missing mandatory type key.",
			http.StatusForbidden,
		)
	}

	updatingObjs := reflect.MakeSlice(reflect.SliceOf(res.resourceType), 1, 1)
	updatingObjs.Index(0).Set(reflect.ValueOf(obj.Result()))

	structType := res.resourceType
	if structType.Kind() == reflect.Ptr {
		structType = structType.Elem()
	}

	err = jsonapi.UnmarshalInto(ctx, structType, &updatingObjs)
	if err != nil {
		return err
	}
	if updatingObjs.Len() != 1 {
		return errors.New("expected one object")
	}

	updatingObj := updatingObjs.Index(0).Interface()

	response, err := res.source.Update(updatingObj, buildRequest(r))

	if err != nil {
		return err
	}

	switch response.StatusCode() {
	case http.StatusOK:
		updated := response.Result()
		if updated == nil {
			internalResponse, err := res.source.FindOne(ps.ByName("id"), buildRequest(r))
			if err != nil {
				return err
			}
			updated = internalResponse.Result()
			if updated == nil {
				return fmt.Errorf("Expected FindOne to return one object of resource %s", res.name)
			}

			response = internalResponse
		}

		return respondWith(response, information{}, http.StatusOK, w, r, res.marshalers)
	case http.StatusAccepted:
		w.WriteHeader(http.StatusAccepted)
		return nil
	case http.StatusNoContent:
		w.WriteHeader(http.StatusNoContent)
		return nil
	default:
		return fmt.Errorf("invalid status code %d from resource %s for method Update", response.StatusCode(), res.name)
	}
}

func (res *resource) handleReplaceRelation(w http.ResponseWriter, r *http.Request, ps httprouter.Params, relation jsonapi.Reference) error {
	var (
		err     error
		editObj interface{}
	)

	response, err := res.source.FindOne(ps.ByName("id"), buildRequest(r))
	if err != nil {
		return err
	}

	inc, err := unmarshalRequest(r, res.marshalers)
	if err != nil {
		return err
	}

	data, ok := inc["data"]
	if !ok {
		return errors.New("Invalid object. Need a \"data\" object")
	}

	resType := reflect.TypeOf(response.Result()).Kind()
	if resType == reflect.Struct {
		editObj = getPointerToStruct(response.Result())
	} else {
		editObj = response.Result()
	}

	err = jsonapi.UnmarshalRelationshipsData(editObj, relation.Name, data)
	if err != nil {
		return err
	}

	if resType == reflect.Struct {
		_, err = res.source.Update(reflect.ValueOf(editObj).Elem().Interface(), buildRequest(r))
	} else {
		_, err = res.source.Update(editObj, buildRequest(r))
	}

	w.WriteHeader(http.StatusNoContent)
	return err
}

func (res *resource) handleAddToManyRelation(w http.ResponseWriter, r *http.Request, ps httprouter.Params, relation jsonapi.Reference) error {
	var (
		err     error
		editObj interface{}
	)

	response, err := res.source.FindOne(ps.ByName("id"), buildRequest(r))
	if err != nil {
		return err
	}

	inc, err := unmarshalRequest(r, res.marshalers)
	if err != nil {
		return err
	}

	data, ok := inc["data"]
	if !ok {
		return errors.New("Invalid object. Need a \"data\" object")
	}

	newRels, ok := data.([]interface{})
	if !ok {
		return fmt.Errorf("Data must be an array with \"id\" and \"type\" field to add new to-many relationships")
	}

	newIDs := []string{}

	for _, newRel := range newRels {
		casted, ok := newRel.(map[string]interface{})
		if !ok {
			return errors.New("entry in data object invalid")
		}
		newID, ok := casted["id"].(string)
		if !ok {
			return errors.New("no id field found inside data object")
		}

		newIDs = append(newIDs, newID)
	}

	resType := reflect.TypeOf(response.Result()).Kind()
	if resType == reflect.Struct {
		editObj = getPointerToStruct(response.Result())
	} else {
		editObj = response.Result()
	}

	targetObj, ok := editObj.(jsonapi.EditToManyRelations)
	if !ok {
		return errors.New("target struct must implement jsonapi.EditToManyRelations")
	}
	targetObj.AddToManyIDs(relation.Name, newIDs)

	if resType == reflect.Struct {
		_, err = res.source.Update(reflect.ValueOf(targetObj).Elem().Interface(), buildRequest(r))
	} else {
		_, err = res.source.Update(targetObj, buildRequest(r))
	}

	w.WriteHeader(http.StatusNoContent)

	return err
}

func (res *resource) handleDeleteToManyRelation(w http.ResponseWriter, r *http.Request, ps httprouter.Params, relation jsonapi.Reference) error {
	var (
		err     error
		editObj interface{}
	)
	response, err := res.source.FindOne(ps.ByName("id"), buildRequest(r))
	if err != nil {
		return err
	}

	inc, err := unmarshalRequest(r, res.marshalers)
	if err != nil {
		return err
	}

	data, ok := inc["data"]
	if !ok {
		return errors.New("Invalid object. Need a \"data\" object")
	}

	newRels, ok := data.([]interface{})
	if !ok {
		return fmt.Errorf("Data must be an array with \"id\" and \"type\" field to add new to-many relationships")
	}

	obsoleteIDs := []string{}

	for _, newRel := range newRels {
		casted, ok := newRel.(map[string]interface{})
		if !ok {
			return errors.New("entry in data object invalid")
		}
		obsoleteID, ok := casted["id"].(string)
		if !ok {
			return errors.New("no id field found inside data object")
		}

		obsoleteIDs = append(obsoleteIDs, obsoleteID)
	}

	resType := reflect.TypeOf(response.Result()).Kind()
	if resType == reflect.Struct {
		editObj = getPointerToStruct(response.Result())
	} else {
		editObj = response.Result()
	}

	targetObj, ok := editObj.(jsonapi.EditToManyRelations)
	if !ok {
		return errors.New("target struct must implement jsonapi.EditToManyRelations")
	}
	targetObj.DeleteToManyIDs(relation.Name, obsoleteIDs)

	if resType == reflect.Struct {
		_, err = res.source.Update(reflect.ValueOf(targetObj).Elem().Interface(), buildRequest(r))
	} else {
		_, err = res.source.Update(targetObj, buildRequest(r))
	}

	w.WriteHeader(http.StatusNoContent)

	return err
}

// returns a pointer to an interface{} struct
func getPointerToStruct(oldObj interface{}) interface{} {
	resType := reflect.TypeOf(oldObj)
	ptr := reflect.New(resType)
	ptr.Elem().Set(reflect.ValueOf(oldObj))
	return ptr.Interface()
}

func (res *resource) handleDelete(w http.ResponseWriter, r *http.Request, ps httprouter.Params) error {
	response, err := res.source.Delete(ps.ByName("id"), buildRequest(r))
	if err != nil {
		return err
	}

	switch response.StatusCode() {
	case http.StatusOK:
		data := map[string]interface{}{
			"meta": response.Metadata(),
		}

		return marshalResponse(data, w, http.StatusOK, r, res.marshalers)
	case http.StatusAccepted:
		w.WriteHeader(http.StatusAccepted)
		return nil
	case http.StatusNoContent:
		w.WriteHeader(http.StatusNoContent)
		return nil
	default:
		return fmt.Errorf("invalid status code %d from resource %s for method Delete", response.StatusCode(), res.name)
	}
}

func writeResult(w http.ResponseWriter, data []byte, status int, contentType string) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	w.Write(data)
}

func respondWith(obj Responder, info information, status int, w http.ResponseWriter, r *http.Request, marshalers map[string]ContentMarshaler) error {
	data, err := jsonapi.MarshalWithURLs(obj.Result(), info)
	if err != nil {
		return err
	}

	meta := obj.Metadata()
	if len(meta) > 0 {
		data["meta"] = meta
	}

	return marshalResponse(data, w, status, r, marshalers)
}

func respondWithPagination(obj Responder, info information, status int, links map[string]string, w http.ResponseWriter, r *http.Request, marshalers map[string]ContentMarshaler) error {
	data, err := jsonapi.MarshalWithURLs(obj.Result(), info)
	if err != nil {
		return err
	}

	data["links"] = links
	meta := obj.Metadata()
	if len(meta) > 0 {
		data["meta"] = meta
	}

	return marshalResponse(data, w, status, r, marshalers)
}

func unmarshalRequest(r *http.Request, marshalers map[string]ContentMarshaler) (map[string]interface{}, error) {
	defer r.Body.Close()
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	result := map[string]interface{}{}
	marshaler, _ := selectContentMarshaler(r, marshalers)
	err = marshaler.Unmarshal(data, &result)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func marshalResponse(resp interface{}, w http.ResponseWriter, status int, r *http.Request, marshalers map[string]ContentMarshaler) error {
	marshaler, contentType := selectContentMarshaler(r, marshalers)
	result, err := marshaler.Marshal(resp)
	if err != nil {
		return err
	}
	writeResult(w, result, status, contentType)
	return nil
}

func selectContentMarshaler(r *http.Request, marshalers map[string]ContentMarshaler) (marshaler ContentMarshaler, contentType string) {
	if _, found := r.Header["Accept"]; found {
		var contentTypes []string
		for ct := range marshalers {
			contentTypes = append(contentTypes, ct)
		}

		contentType = httputil.NegotiateContentType(r, contentTypes, defaultContentTypHeader)
		marshaler = marshalers[contentType]
	} else if contentTypes, found := r.Header["Content-Type"]; found {
		contentType = contentTypes[0]
		marshaler = marshalers[contentType]
	}

	if marshaler == nil {
		contentType = defaultContentTypHeader
		marshaler = JSONContentMarshaler{}
	}

	return
}

func handleError(err error, w http.ResponseWriter, r *http.Request, marshalers map[string]ContentMarshaler) {
	marshaler, contentType := selectContentMarshaler(r, marshalers)

	log.Println(err)
	if e, ok := err.(HTTPError); ok {
		writeResult(w, []byte(marshaler.MarshalError(err)), e.status, contentType)
		return

	}

	writeResult(w, []byte(marshaler.MarshalError(err)), http.StatusInternalServerError, contentType)
}
