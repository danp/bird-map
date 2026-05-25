# Bird Halifax Parking Map

This repo generates a GitHub Pages site showing:

- designated Bird parking zones in Halifax
- vehicles outside designated parking zones
- the top 20 vehicles furthest from any designated parking zone
- how long each vehicle has remained in the same spot

## Local run

```sh
go run .
```

Generated files land in `output/`:

- `output/index.html`
- `output/vehicles_outside_parking.geojson`
- `output/top_20_furthest_vehicles.geojson`
- `output/vehicle_state.json`

For local runs, the generator reuses `output/vehicle_state.json` when present so the dwell timer carries forward between runs. On GitHub Pages builds, the app derives the current Pages URL from the repository metadata and merges the last published `vehicle_state.json` into the new snapshot. Set `PUBLISHED_SITE_URL` or `PUBLISHED_STATE_URL` if you need to override that, such as with a custom domain.

## GitHub Pages deployment

The repo includes a GitHub Actions workflow at `.github/workflows/pages.yml` that:

- builds the site on pushes to `main`
- deploys to GitHub Pages
- refreshes the published page every hour at minute 17
- supports manual runs from the Actions tab

One-time GitHub setup:

1. Push the repo to GitHub.
2. In `Settings -> Pages`, set `Source` to `GitHub Actions`.
3. Keep the default branch as `main`, or update the workflow if you use a different branch.
