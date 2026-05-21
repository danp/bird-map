# Bird Halifax Parking Map

This repo generates a GitHub Pages site showing:

- designated Bird parking zones in Halifax
- vehicles outside designated parking zones
- the top 20 vehicles furthest from any designated parking zone

## Local run

```sh
go run .
```

Generated files land in `output/`:

- `output/index.html`
- `output/vehicles_outside_parking.geojson`

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
