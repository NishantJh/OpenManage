package main

import "testing"

func TestFloorLayoutValidate(t *testing.T) {
	good := floorLayout{
		Rooms:      []floorRoom{{ID: "bed", Name: " Bedroom ", X: 0, Y: 0, W: 4, H: 3}},
		Placements: []floorPlacement{{DeviceID: "11111111-1111-4111-8111-111111111111", X: 2, Y: 1}},
	}
	if err := good.validate(); err != nil || good.Rooms[0].Name != "Bedroom" {
		t.Fatalf("good layout rejected or name not trimmed: %v", err)
	}
	empty := floorLayout{}
	if err := empty.validate(); err != nil || empty.Rooms == nil || empty.Placements == nil {
		t.Fatalf("empty layout should normalise to empty lists: %v", err)
	}
	bad := map[string]floorLayout{
		"duplicate room": {Rooms: []floorRoom{{ID: "a", Name: "A", W: 1, H: 1}, {ID: "a", Name: "B", W: 1, H: 1}}},
		"bad room id":    {Rooms: []floorRoom{{ID: "a b", Name: "A", W: 1, H: 1}}},
		"blank name":     {Rooms: []floorRoom{{ID: "a", Name: "  ", W: 1, H: 1}}},
		"tiny room":      {Rooms: []floorRoom{{ID: "a", Name: "A", W: 0.1, H: 1}}},
		"far room":       {Rooms: []floorRoom{{ID: "a", Name: "A", X: 9999, W: 1, H: 1}}},
		"bad device":     {Placements: []floorPlacement{{DeviceID: "nope"}}},
		"dup placement": {Placements: []floorPlacement{
			{DeviceID: "11111111-1111-4111-8111-111111111111"}, {DeviceID: "11111111-1111-4111-8111-111111111111"}}},
		"negative placement": {Placements: []floorPlacement{{DeviceID: "11111111-1111-4111-8111-111111111111", X: -1}}},
	}
	for name, l := range bad {
		if err := l.validate(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestFloorLayoutImageValidate(t *testing.T) {
	ok := floorLayout{Image: &floorImage{X: 1, Y: 2, WidthM: 20}}
	if err := ok.validate(); err != nil {
		t.Fatalf("valid image placement rejected: %v", err)
	}
	for name, img := range map[string]floorImage{
		"tiny":     {WidthM: 0.1},
		"huge":     {WidthM: 9999},
		"negative": {X: -1, WidthM: 5},
		"far":      {Y: 9999, WidthM: 5},
	} {
		l := floorLayout{Image: &img}
		if err := l.validate(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
