package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	gbfsURL           = "https://mds.bird.co/gbfs/v2/public/halifax/gbfs.json"
	outputDir         = "output"
	geoJSONName       = "vehicles_outside_parking.geojson"
	parkingZonesName  = "parking_zones.geojson"
	top20VehiclesName = "top_20_furthest_vehicles.geojson"
	htmlMapName       = "index.html"
	requestTimeout    = 20 * time.Second
)

type gbfsRoot struct {
	Data map[string]struct {
		Feeds []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"feeds"`
	} `json:"data"`
}

type freeBikeStatus struct {
	Data struct {
		Bikes []vehicle `json:"bikes"`
	} `json:"data"`
	LastUpdated int64 `json:"last_updated"`
}

type vehicle struct {
	BikeID        string  `json:"bike_id"`
	Lat           float64 `json:"lat"`
	Lon           float64 `json:"lon"`
	IsDisabled    bool    `json:"is_disabled"`
	IsReserved    bool    `json:"is_reserved"`
	VehicleTypeID string  `json:"vehicle_type_id"`
	LastReported  int64   `json:"last_reported"`
}

type geofencingZones struct {
	Data struct {
		GeofencingZones struct {
			Features []feature `json:"features"`
		} `json:"geofencing_zones"`
	} `json:"data"`
}

type feature struct {
	Type       string         `json:"type"`
	Properties zoneProperties `json:"properties"`
	Geometry   geometry       `json:"geometry"`
}

type zoneProperties struct {
	Rules []zoneRule `json:"rules"`
}

type zoneRule struct {
	StationParking bool `json:"station_parking"`
}

type geometry struct {
	Type        string          `json:"type"`
	Coordinates json.RawMessage `json:"coordinates"`
}

type vehicleTypes struct {
	Data struct {
		VehicleTypes []struct {
			VehicleTypeID string `json:"vehicle_type_id"`
			FormFactor    string `json:"form_factor"`
		} `json:"vehicle_types"`
	} `json:"data"`
}

type point struct {
	Lon float64
	Lat float64
}

type polygon []point
type multiPolygon []polygon

type namedZone struct {
	StationID string
	Name      string
	Geometry  multiPolygon
	Feature   outputFeature
}

type rankedVehicle struct {
	Vehicle         vehicle
	DistanceMeters  float64
	NearestZoneName string
}

type outputFeatureCollection struct {
	Type     string          `json:"type"`
	Features []outputFeature `json:"features"`
}

type outputFeature struct {
	Type       string                 `json:"type"`
	Geometry   outputGeometry         `json:"geometry"`
	Properties map[string]interface{} `json:"properties"`
}

type outputGeometry struct {
	Type        string      `json:"type"`
	Coordinates interface{} `json:"coordinates"`
}

type mapTemplateData struct {
	GeneratedAt       string
	LastUpdated       string
	TotalVehicles     int
	OutsideVehicles   int
	Top20Vehicles     int
	ParkingZonesPath  string
	OutsidePointsPath string
	Top20PointsPath   string
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	feedIndex := gbfsRoot{}
	if err := fetchJSON(ctx, gbfsURL, &feedIndex); err != nil {
		exitf("fetch gbfs index: %v", err)
	}

	feedURLs, err := indexFeeds(feedIndex)
	if err != nil {
		exitf("read feed index: %v", err)
	}

	var bikes freeBikeStatus
	if err := fetchJSON(ctx, feedURLs["free_bike_status"], &bikes); err != nil {
		exitf("fetch free_bike_status: %v", err)
	}

	var zones geofencingZones
	if err := fetchJSON(ctx, feedURLs["geofencing_zones"], &zones); err != nil {
		exitf("fetch geofencing_zones: %v", err)
	}

	var stations stationInformation
	if err := fetchJSON(ctx, feedURLs["station_information"], &stations); err != nil {
		exitf("fetch station_information: %v", err)
	}

	var types vehicleTypes
	if err := fetchJSON(ctx, feedURLs["vehicle_types"], &types); err != nil {
		exitf("fetch vehicle_types: %v", err)
	}

	typeNames := map[string]string{}
	for _, vt := range types.Data.VehicleTypes {
		typeNames[vt.VehicleTypeID] = vt.FormFactor
	}

	parkingZones, _, err := extractParkingZones(zones.Data.GeofencingZones.Features)
	if err != nil {
		exitf("extract parking zones: %v", err)
	}

	stationZones, rawStationFeatures, err := extractStationZones(stations.Data.Stations)
	if err != nil {
		exitf("extract station zones: %v", err)
	}

	outsideGeofenced := make([]vehicle, 0)
	for _, bike := range bikes.Data.Bikes {
		if !pointInAnyZone(point{Lon: bike.Lon, Lat: bike.Lat}, parkingZones) {
			outsideGeofenced = append(outsideGeofenced, bike)
		}
	}

	outsideStations := make([]rankedVehicle, 0)
	allRanked := make([]rankedVehicle, 0, len(bikes.Data.Bikes))
	for _, bike := range bikes.Data.Bikes {
		ranked := rankVehicle(bike, stationZones)
		allRanked = append(allRanked, ranked)
		if ranked.DistanceMeters > 0 {
			outsideStations = append(outsideStations, ranked)
		}
	}
	sort.Slice(allRanked, func(i, j int) bool {
		return allRanked[i].DistanceMeters > allRanked[j].DistanceMeters
	})
	top20 := allRanked
	if len(top20) > 20 {
		top20 = top20[:20]
	}

	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		exitf("create output dir: %v", err)
	}

	geoJSONPath := filepath.Join(outputDir, geoJSONName)
	if err := writeOutsideGeoJSON(geoJSONPath, outsideStations, typeNames); err != nil {
		exitf("write geojson: %v", err)
	}

	parkingZonesPath := filepath.Join(outputDir, parkingZonesName)
	if err := writeFeatureCollection(parkingZonesPath, outputFeatureCollection{
		Type:     "FeatureCollection",
		Features: rawStationFeatures,
	}); err != nil {
		exitf("write parking zones geojson: %v", err)
	}

	top20Path := filepath.Join(outputDir, top20VehiclesName)
	if err := writeFeatureCollection(top20Path, outputFeatureCollection{
		Type:     "FeatureCollection",
		Features: convertOutsideFeatures(top20, typeNames),
	}); err != nil {
		exitf("write top 20 geojson: %v", err)
	}

	htmlPath := filepath.Join(outputDir, htmlMapName)
	if err := writeHTMLMap(htmlPath, bikes.LastUpdated, len(bikes.Data.Bikes), len(outsideStations), len(top20)); err != nil {
		exitf("write html map: %v", err)
	}

	fmt.Printf("Generated %s and %s\n", geoJSONPath, htmlPath)
	fmt.Printf("Vehicles outside designated station parking zones: %d of %d\n", len(outsideStations), len(bikes.Data.Bikes))
	fmt.Printf("Vehicles outside geofenced parking-allowed zones: %d of %d\n", len(outsideGeofenced), len(bikes.Data.Bikes))
}

func fetchJSON(ctx context.Context, url string, dst interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("unexpected status %s: %s", resp.Status, string(body))
	}

	return json.NewDecoder(resp.Body).Decode(dst)
}

func indexFeeds(root gbfsRoot) (map[string]string, error) {
	for _, localeData := range root.Data {
		feeds := map[string]string{}
		for _, feed := range localeData.Feeds {
			feeds[feed.Name] = feed.URL
		}
		for _, required := range []string{"free_bike_status", "geofencing_zones", "vehicle_types"} {
			if required == "station_information" {
				continue
			}
			if feeds[required] == "" {
				return nil, fmt.Errorf("missing required feed %q", required)
			}
		}
		for _, required := range []string{"station_information"} {
			if feeds[required] == "" {
				return nil, fmt.Errorf("missing required feed %q", required)
			}
		}
		return feeds, nil
	}
	return nil, fmt.Errorf("no locale data in GBFS index")
}

type stationInformation struct {
	Data struct {
		Stations []station `json:"stations"`
	} `json:"data"`
}

type station struct {
	StationID   string   `json:"station_id"`
	Name        string   `json:"name"`
	Lat         float64  `json:"lat"`
	Lon         float64  `json:"lon"`
	StationArea geometry `json:"station_area"`
}

func extractParkingZones(features []feature) ([]multiPolygon, []feature, error) {
	var zones []multiPolygon
	var raw []feature

	for _, feature := range features {
		if !hasStationParking(feature.Properties.Rules) {
			continue
		}

		mp, err := parseMultiPolygon(feature.Geometry)
		if err != nil {
			return nil, nil, err
		}

		zones = append(zones, mp)
		raw = append(raw, feature)
	}

	return zones, raw, nil
}

func extractStationZones(stations []station) ([]namedZone, []outputFeature, error) {
	var zones []namedZone
	var raw []outputFeature

	for _, station := range stations {
		mp, err := parseMultiPolygon(station.StationArea)
		if err != nil {
			return nil, nil, fmt.Errorf("station %s: %w", station.StationID, err)
		}

		var coords interface{}
		if err := json.Unmarshal(station.StationArea.Coordinates, &coords); err != nil {
			return nil, nil, err
		}
		coords = roundNestedCoordinates(coords, 6)

		feature := outputFeature{
			Type: "Feature",
			Geometry: outputGeometry{
				Type:        station.StationArea.Type,
				Coordinates: coords,
			},
			Properties: map[string]interface{}{
				"station_id": station.StationID,
				"name":       station.Name,
			},
		}
		zones = append(zones, namedZone{
			StationID: station.StationID,
			Name:      station.Name,
			Geometry:  mp,
			Feature:   feature,
		})
		raw = append(raw, feature)
	}

	return zones, raw, nil
}

func hasStationParking(rules []zoneRule) bool {
	for _, rule := range rules {
		if rule.StationParking {
			return true
		}
	}
	return false
}

func parseMultiPolygon(g geometry) (multiPolygon, error) {
	switch g.Type {
	case "MultiPolygon":
		var coords [][][][]float64
		if err := json.Unmarshal(g.Coordinates, &coords); err != nil {
			return nil, err
		}
		var mp multiPolygon
		for _, polyGroup := range coords {
			if len(polyGroup) == 0 {
				continue
			}
			mp = append(mp, ringToPolygon(polyGroup[0]))
		}
		return mp, nil
	case "Polygon":
		var coords [][][]float64
		if err := json.Unmarshal(g.Coordinates, &coords); err != nil {
			return nil, err
		}
		if len(coords) == 0 {
			return nil, nil
		}
		return multiPolygon{ringToPolygon(coords[0])}, nil
	default:
		return nil, fmt.Errorf("unsupported geometry type %q", g.Type)
	}
}

func ringToPolygon(ring [][]float64) polygon {
	poly := make(polygon, 0, len(ring))
	for _, coord := range ring {
		if len(coord) < 2 {
			continue
		}
		poly = append(poly, point{Lon: coord[0], Lat: coord[1]})
	}
	return poly
}

func pointInAnyZone(pt point, zones []multiPolygon) bool {
	for _, zone := range zones {
		for _, poly := range zone {
			if pointInPolygon(pt, poly) {
				return true
			}
		}
	}
	return false
}

func rankVehicle(bike vehicle, zones []namedZone) rankedVehicle {
	pt := point{Lon: bike.Lon, Lat: bike.Lat}
	best := rankedVehicle{
		Vehicle:        bike,
		DistanceMeters: math.MaxFloat64,
	}

	for _, zone := range zones {
		dist := distanceToZoneMeters(pt, zone.Geometry)
		if dist < best.DistanceMeters {
			best.DistanceMeters = dist
			best.NearestZoneName = zone.Name
		}
	}

	if best.DistanceMeters == math.MaxFloat64 {
		best.DistanceMeters = 0
	}

	return best
}

func distanceToZoneMeters(pt point, zone multiPolygon) float64 {
	best := math.MaxFloat64
	for _, poly := range zone {
		dist := distanceToPolygonMeters(pt, poly)
		if dist < best {
			best = dist
		}
	}
	return best
}

func distanceToPolygonMeters(pt point, poly polygon) float64 {
	if len(poly) < 2 {
		return math.MaxFloat64
	}
	if pointInPolygon(pt, poly) {
		return 0
	}

	best := math.MaxFloat64
	for i := 0; i < len(poly); i++ {
		a := poly[i]
		b := poly[(i+1)%len(poly)]
		dist := distancePointToSegmentMeters(pt, a, b)
		if dist < best {
			best = dist
		}
	}
	return best
}

func distancePointToSegmentMeters(pt, a, b point) float64 {
	lat0 := pt.Lat * math.Pi / 180
	cosLat := math.Cos(lat0)
	const metersPerDegreeLat = 111320.0

	toXY := func(p point) (float64, float64) {
		x := (p.Lon - pt.Lon) * cosLat * metersPerDegreeLat
		y := (p.Lat - pt.Lat) * metersPerDegreeLat
		return x, y
	}

	ax, ay := toXY(a)
	bx, by := toXY(b)
	dx, dy := bx-ax, by-ay
	den := dx*dx + dy*dy
	if den == 0 {
		return math.Hypot(ax, ay)
	}

	t := -(ax*dx + ay*dy) / den
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}

	px := ax + t*dx
	py := ay + t*dy
	return math.Hypot(px, py)
}

func roundNestedCoordinates(v interface{}, places int) interface{} {
	switch val := v.(type) {
	case []interface{}:
		out := make([]interface{}, len(val))
		for i, child := range val {
			out[i] = roundNestedCoordinates(child, places)
		}
		return out
	case float64:
		factor := math.Pow(10, float64(places))
		return math.Round(val*factor) / factor
	default:
		return v
	}
}

func pointInPolygon(pt point, poly polygon) bool {
	if len(poly) < 3 {
		return false
	}

	inside := false
	j := len(poly) - 1
	for i := 0; i < len(poly); i++ {
		pi := poly[i]
		pj := poly[j]

		onSegment := math.Abs(cross(pj, pi, pt)) < 1e-12 &&
			math.Min(pi.Lon, pj.Lon)-1e-12 <= pt.Lon && pt.Lon <= math.Max(pi.Lon, pj.Lon)+1e-12 &&
			math.Min(pi.Lat, pj.Lat)-1e-12 <= pt.Lat && pt.Lat <= math.Max(pi.Lat, pj.Lat)+1e-12
		if onSegment {
			return true
		}

		intersects := ((pi.Lat > pt.Lat) != (pj.Lat > pt.Lat)) &&
			(pt.Lon < (pj.Lon-pi.Lon)*(pt.Lat-pi.Lat)/(pj.Lat-pi.Lat)+pi.Lon)
		if intersects {
			inside = !inside
		}
		j = i
	}

	return inside
}

func cross(a, b, c point) float64 {
	return (b.Lon-a.Lon)*(c.Lat-a.Lat) - (b.Lat-a.Lat)*(c.Lon-a.Lon)
}

func writeOutsideGeoJSON(path string, outside []rankedVehicle, typeNames map[string]string) error {
	fc := outputFeatureCollection{
		Type:     "FeatureCollection",
		Features: make([]outputFeature, 0, len(outside)),
	}

	for _, bike := range outside {
		fc.Features = append(fc.Features, outputFeature{
			Type: "Feature",
			Geometry: outputGeometry{
				Type:        "Point",
				Coordinates: []float64{bike.Vehicle.Lon, bike.Vehicle.Lat},
			},
			Properties: map[string]interface{}{
				"bike_id":           bike.Vehicle.BikeID,
				"vehicle_type_id":   bike.Vehicle.VehicleTypeID,
				"vehicle_type":      typeNames[bike.Vehicle.VehicleTypeID],
				"is_disabled":       bike.Vehicle.IsDisabled,
				"is_reserved":       bike.Vehicle.IsReserved,
				"last_reported":     bike.Vehicle.LastReported,
				"distance_meters":   bike.DistanceMeters,
				"nearest_zone_name": bike.NearestZoneName,
			},
		})
	}

	return writeFeatureCollection(path, fc)
}

func writeFeatureCollection(path string, fc outputFeatureCollection) error {
	data, err := json.Marshal(fc)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func writeHTMLMap(path string, lastUpdated int64, total int, outsideCount int, top20Count int) error {
	data := mapTemplateData{
		GeneratedAt:       time.Now().Format(time.RFC3339),
		LastUpdated:       time.Unix(lastUpdated, 0).Format(time.RFC3339),
		TotalVehicles:     total,
		OutsideVehicles:   outsideCount,
		Top20Vehicles:     top20Count,
		ParkingZonesPath:  parkingZonesName,
		OutsidePointsPath: geoJSONName,
		Top20PointsPath:   top20VehiclesName,
	}

	tpl, err := template.New("map").Parse(htmlTemplate)
	if err != nil {
		return err
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return tpl.Execute(f, data)
}

func convertOutsideFeatures(outside []rankedVehicle, typeNames map[string]string) []outputFeature {
	out := make([]outputFeature, 0, len(outside))
	for idx, bike := range outside {
		out = append(out, outputFeature{
			Type: "Feature",
			Geometry: outputGeometry{
				Type:        "Point",
				Coordinates: []float64{bike.Vehicle.Lon, bike.Vehicle.Lat},
			},
			Properties: map[string]interface{}{
				"bike_id":           bike.Vehicle.BikeID,
				"vehicle_type":      typeNames[bike.Vehicle.VehicleTypeID],
				"is_disabled":       bike.Vehicle.IsDisabled,
				"is_reserved":       bike.Vehicle.IsReserved,
				"last_reported":     bike.Vehicle.LastReported,
				"distance_meters":   bike.DistanceMeters,
				"nearest_zone_name": bike.NearestZoneName,
				"rank":              idx + 1,
			},
		})
	}
	return out
}

func exitf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

const htmlTemplate = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Bird Halifax Vehicles and Parking Zones</title>
  <link rel="stylesheet" href="https://unpkg.com/leaflet@1.9.4/dist/leaflet.css" integrity="sha256-p4NxAoJBhIIN+hmNHrzRCf9tD/miZyoHS5obTRR9BMY=" crossorigin="">
  <style>
    :root {
      color-scheme: light;
      --bg: #f4f1e8;
      --ink: #122620;
      --accent: #d64933;
      --zone: #00798c;
      --panel: rgba(255,255,255,0.92);
    }
    body {
      margin: 0;
      font-family: Georgia, "Times New Roman", serif;
      color: var(--ink);
      background:
        radial-gradient(circle at top left, rgba(214,73,51,0.15), transparent 35%),
        linear-gradient(180deg, #efe8d8 0%, var(--bg) 100%);
    }
    #map {
      height: 100vh;
      width: 100vw;
    }
    .panel {
      position: absolute;
      top: 16px;
      left: 16px;
      z-index: 1000;
      max-width: 320px;
      padding: 14px 16px;
      background: var(--panel);
      border: 1px solid rgba(18,38,32,0.12);
      box-shadow: 0 10px 30px rgba(18,38,32,0.12);
      backdrop-filter: blur(8px);
    }
    .panel h1 {
      margin: 0 0 8px;
      font-size: 1.15rem;
    }
    .panel p {
      margin: 0.35rem 0;
      line-height: 1.35;
      font-size: 0.95rem;
    }
    .panel .hint {
      color: rgba(18,38,32,0.78);
      font-size: 0.85rem;
    }
    .legend-dot {
      display: inline-block;
      width: 10px;
      height: 10px;
      border-radius: 999px;
      margin-right: 6px;
      vertical-align: middle;
    }
  </style>
</head>
<body>
  <div class="panel">
    <h1>Bird Halifax: vehicles and parking zones</h1>
    <p><strong>{{.OutsideVehicles}}</strong> of <strong>{{.TotalVehicles}}</strong> live vehicles are outside a designated parking zone.</p>
    <p><strong>{{.Top20Vehicles}}</strong> vehicles are highlighted as the furthest from any designated parking zone.</p>
    <p>Feed updated: {{.LastUpdated}}</p>
    <p>Generated: {{.GeneratedAt}}</p>
    <p><span class="legend-dot" style="background:#00798c;"></span>Designated parking zone</p>
    <p><span class="legend-dot" style="background:#f2c14e;"></span>Parking zone center marker</p>
    <p><span class="legend-dot" style="background:#d64933;"></span>Vehicle outside parking zone</p>
    <p><span class="legend-dot" style="background:#111827;"></span>Top 20 furthest vehicles</p>
    <p class="hint">Click a teal zone or gold marker to see the parking location name. Click a red or black marker for distance details.</p>
  </div>
  <div id="map"></div>
  <script src="https://unpkg.com/leaflet@1.9.4/dist/leaflet.js" integrity="sha256-20nQCchB9co0qIjJZRGuk2/Z9VM+kNiyxNV1lvTlZBo=" crossorigin=""></script>
  <script>
    const parkingZonesPath = "{{.ParkingZonesPath}}";
    const outsideVehiclesPath = "{{.OutsidePointsPath}}";
    const top20VehiclesPath = "{{.Top20PointsPath}}";

    const map = L.map('map');
    L.tileLayer('https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png', {
      maxZoom: 19,
      attribution: '&copy; OpenStreetMap contributors'
    }).addTo(map);

    function extendBounds(bounds, layer) {
      if (layer.getBounds) {
        const layerBounds = layer.getBounds();
        if (layerBounds.isValid()) {
          bounds.extend(layerBounds);
        }
        return;
      }
      if (layer.eachLayer) {
        layer.eachLayer((child) => {
          if (child.getBounds) {
            const childBounds = child.getBounds();
            if (childBounds.isValid()) {
              bounds.extend(childBounds);
            }
            return;
          }
          if (child.getLatLng) {
            bounds.extend(child.getLatLng());
          }
        });
        return;
      }
      if (layer.getLatLng) {
        bounds.extend(layer.getLatLng());
      }
    }

    Promise.all([
      fetch(parkingZonesPath).then((response) => response.json()),
      fetch(outsideVehiclesPath).then((response) => response.json()),
      fetch(top20VehiclesPath).then((response) => response.json())
    ]).then(([parkingZones, outsideVehicles, top20Vehicles]) => {
      const zoneLayer = L.geoJSON(parkingZones, {
        style: {
          color: '#00798c',
          weight: 3,
          fillColor: '#00798c',
          fillOpacity: 0.22
        },
        onEachFeature: (feature, layer) => {
          const props = feature.properties || {};
          if (props.name) {
            layer.bindPopup('<strong>Designated parking zone</strong><br>' + props.name);
          }
        }
      }).addTo(map);

      const zoneCenterLayer = L.layerGroup();
      zoneLayer.eachLayer((layer) => {
        if (!layer.getBounds) {
          return;
        }
        const center = layer.getBounds().getCenter();
        const props = layer.feature && layer.feature.properties ? layer.feature.properties : {};
        const marker = L.circleMarker(center, {
          radius: 4,
          color: '#7d5a00',
          weight: 1,
          fillColor: '#f2c14e',
          fillOpacity: 0.95
        });
        if (props.name) {
          marker.bindPopup('<strong>Designated parking zone</strong><br>' + props.name);
        }
        zoneCenterLayer.addLayer(marker);
      });
      zoneCenterLayer.addTo(map);

      const markerLayer = L.geoJSON(outsideVehicles, {
        pointToLayer: (feature, latlng) => L.circleMarker(latlng, {
          radius: 5,
          color: '#7a1f14',
          weight: 1,
          fillColor: '#d64933',
          fillOpacity: 0.95
        }),
        onEachFeature: (feature, layer) => {
          const props = feature.properties;
          layer.bindPopup(
            '<strong>' + props.vehicle_type + '</strong><br>' +
            'Vehicle ID: ' + props.bike_id + '<br>' +
            'Nearest zone: ' + props.nearest_zone_name + '<br>' +
            'Distance to nearest zone: ' + Math.round(props.distance_meters) + ' m<br>' +
            'Disabled: ' + props.is_disabled + '<br>' +
            'Reserved: ' + props.is_reserved + '<br>' +
            'Last reported: ' + new Date(props.last_reported * 1000).toISOString()
          );
        }
      }).addTo(map);

      const top20Layer = L.geoJSON(top20Vehicles, {
        pointToLayer: (feature, latlng) => L.circleMarker(latlng, {
          radius: 7,
          color: '#ffffff',
          weight: 2,
          fillColor: '#111827',
          fillOpacity: 0.95
        }),
        onEachFeature: (feature, layer) => {
          const props = feature.properties;
          layer.bindPopup(
            '<strong>#' + props.rank + ' furthest vehicle</strong><br>' +
            'Type: ' + props.vehicle_type + '<br>' +
            'Vehicle ID: ' + props.bike_id + '<br>' +
            'Nearest zone: ' + props.nearest_zone_name + '<br>' +
            'Distance to nearest zone: ' + Math.round(props.distance_meters) + ' m<br>' +
            'Disabled: ' + props.is_disabled + '<br>' +
            'Reserved: ' + props.is_reserved + '<br>' +
            'Last reported: ' + new Date(props.last_reported * 1000).toISOString()
          );
        }
      }).addTo(map);

      L.control.layers(null, {
        'Designated parking polygons': zoneLayer,
        'Parking zone center markers': zoneCenterLayer,
        'Vehicles outside parking zones': markerLayer,
        'Top 20 furthest vehicles': top20Layer
      }, {
        collapsed: false
      }).addTo(map);

      const bounds = L.latLngBounds([]);
      [zoneLayer, zoneCenterLayer, markerLayer, top20Layer].forEach((layer) => extendBounds(bounds, layer));
      if (bounds.isValid()) {
        map.fitBounds(bounds.pad(0.03));
      } else {
        map.setView([44.6488, -63.5752], 13);
      }
    }).catch((error) => {
      console.error(error);
      map.setView([44.6488, -63.5752], 13);
    });
  </script>
  <script data-goatcounter="https://s.danp.net/count" async src="//s.danp.net/count.js"></script>
</body>
</html>
`
