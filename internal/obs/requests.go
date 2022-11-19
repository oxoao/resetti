package obs

// TODO: Add support for request batching?
// TODO: Create a cache of scene item -> ID

import (
	"encoding/json"
)

type Transform struct {
	SourceWidth  float64 `json:"sourceWidth"`
	SourceHeight float64 `json:"sourceHeight"`
	Width        float64 `json:"width"`
	Height       float64 `json:"height"`
	X            float64 `json:"positionX"`
	Y            float64 `json:"positionY"`
}

func (c *Client) AddSceneItem(scene string, source string) error {
	type request struct {
		Scene  string `json:"sceneName"`
		Source string `json:"sourceName"`
	}
	req := request{
		Scene:  scene,
		Source: source,
	}
	_, err := c.request(req, "AddSceneItem")
	return err
}

func (c *Client) CreateSceneCollection(name string) error {
	type request struct {
		SceneCollection string `json:"sceneCollectionName"`
	}
	req := request{
		SceneCollection: name,
	}
	_, err := c.request(req, "CreateSceneCollection")
	return err
}

func (c *Client) CreateScene(name string) error {
	type request struct {
		Scene string `json:"sceneName"`
	}
	req := request{
		Scene: name,
	}
	_, err := c.request(req, "CreateScene")
	return err
}

func (c *Client) CreateSource(scene string, name string, kind string, settings StringMap) error {
	type request struct {
		Scene    string    `json:"sceneName"`
		Input    string    `json:"inputName"`
		Kind     string    `json:"inputKind"`
		Settings StringMap `json:"inputSettings"`
	}
	req := request{
		Scene:    scene,
		Input:    name,
		Kind:     kind,
		Settings: settings,
	}
	_, err := c.request(req, "CreateInput")
	return err
}

func (c *Client) DeleteScene(scene string) error {
	type request struct {
		Scene string `json:"sceneName"`
	}
	req := request{
		Scene: scene,
	}
	_, err := c.request(req, "RemoveScene")
	return err
}

func (c *Client) GetCanvasSize() (width int, height int, err error) {
	raw, err := c.request(nil, "GetVideoSettings")
	if err != nil {
		return 0, 0, err
	}
	res := struct {
		Width  int `json:"baseWidth"`
		Height int `json:"baseHeight"`
	}{}
	err = json.Unmarshal(raw, &res)
	return res.Width, res.Height, err
}

func (c *Client) GetSceneCollectionList() (names []string, active string, err error) {
	raw, err := c.request(nil, "GetSceneCollectionList")
	if err != nil {
		return nil, "", err
	}
	res := struct {
		Collections []string `json:"sceneCollections"`
		Current     string   `json:"currentSceneCollectionName"`
	}{}
	err = json.Unmarshal(raw, &res)
	return res.Collections, res.Current, err
}

func (c *Client) GetSceneItemId(scene string, name string) (int, error) {
	type request struct {
		Scene string `json:"sceneName"`
		Name  string `json:"sourceName"`
	}
	req := request{
		Scene: scene,
		Name:  name,
	}
	raw, err := c.request(req, "GetSceneItemId")
	if err != nil {
		return 0, err
	}
	res := struct {
		Id int `json:"sceneItemId"`
	}{}
	err = json.Unmarshal(raw, &res)
	if err != nil {
		return 0, err
	}
	return res.Id, nil
}

func (c *Client) GetSceneItemTransform(scene string, name string) (Transform, error) {
	type request struct {
		Scene string `json:"sceneName"`
		Item  int    `json:"sceneItemId"`
	}
	id, err := c.GetSceneItemId(scene, name)
	if err != nil {
		return Transform{}, err
	}
	req := request{
		Scene: scene,
		Item:  id,
	}
	raw, err := c.request(req, "GetSceneItemTransform")
	if err != nil {
		return Transform{}, err
	}
	res := struct {
		Transform Transform `json:"sceneItemTransform"`
	}{}
	err = json.Unmarshal(raw, &res)
	return res.Transform, err
}

func (c *Client) GetSourceSettings(name string) (StringMap, error) {
	type request struct {
		Name string `json:"inputName"`
	}
	req := request{name}
	raw, err := c.request(req, "GetInputSettings")
	if err != nil {
		return nil, err
	}
	res := struct {
		Settings StringMap `json:"inputSettings"`
	}{}
	err = json.Unmarshal(raw, &res)
	return res.Settings, err
}

func (c *Client) SetScene(scene string) error {
	type request struct {
		Name string `json:"sceneName"`
	}
	req := request{
		Name: scene,
	}
	_, err := c.request(req, "SetCurrentProgramScene")
	return err
}

func (c *Client) SetSceneCollection(collection string) error {
	type request struct {
		Name string `json:"sceneCollectionName"`
	}
	req := request{
		Name: collection,
	}
	_, err := c.request(req, "SetCurrentSceneCollection")
	return err
}

func (c *Client) SetSceneItemLocked(scene string, name string, locked bool) error {
	type request struct {
		Scene  string `json:"sceneName"`
		Item   int    `json:"sceneItemId"`
		Locked bool   `json:"sceneItemLocked"`
	}
	id, err := c.GetSceneItemId(scene, name)
	if err != nil {
		return err
	}
	req := request{
		Scene:  scene,
		Item:   id,
		Locked: locked,
	}
	_, err = c.request(req, "SetSceneItemLocked")
	return err
}

func (c *Client) SetSceneItemTransform(scene string, name string, transform Transform) error {
	type request struct {
		Scene     string    `json:"sceneName"`
		Item      int       `json:"sceneItemId"`
		Transform Transform `json:"sceneItemTransform"`
	}
	id, err := c.GetSceneItemId(scene, name)
	if err != nil {
		return err
	}
	req := request{
		Scene:     scene,
		Item:      id,
		Transform: transform,
	}
	_, err = c.request(req, "SetSceneItemTransform")
	return err
}

func (c *Client) SetSceneItemVisible(scene string, name string, visible bool) error {
	type request struct {
		Scene   string `json:"sceneName"`
		Item    int    `json:"sceneItemId"`
		Enabled bool   `json:"sceneItemEnabled"`
	}
	id, err := c.GetSceneItemId(scene, name)
	if err != nil {
		return err
	}
	req := request{
		Scene:   scene,
		Item:    id,
		Enabled: visible,
	}
	_, err = c.request(req, "SetSceneItemEnabled")
	return err
}

func (c *Client) SetSourceSettings(name string, settings StringMap, keep bool) error {
	type request struct {
		Name     string    `json:"inputName"`
		Settings StringMap `json:"inputSettings"`
		Reset    bool      `json:"overlay"`
	}
	req := request{
		Name:     name,
		Settings: settings,
		Reset:    keep,
	}
	_, err := c.request(req, "SetInputSettings")
	return err
}