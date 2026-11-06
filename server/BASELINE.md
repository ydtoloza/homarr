# Baseline de recursos: upstream vs homarr-go

## Método

Misma máquina, mismo volumen de datos (~2 boards, 5 apps, 3 widgets activos),
medido tras 5 minutos de calma (sin abrir la UI):

```bash
docker run -d --name homarr-baseline -p 7575:7575 -v homarr-appdata:/appdata \
  ghcr.io/homarr-labs/homarr:latest
sleep 300
docker stats --no-stream --format "{{.Name}}\t{{.MemUsage}}\t{{.CPUPerc}}" homarr-baseline
```

Y para el fork (cuando haya imagen):

```bash
docker run -d --name homarr-go-baseline -p 7576:7575 -v homarr-go-appdata:/appdata \
  ghcr.io/ydtoloza/homarr-go:dev
sleep 60
docker stats --no-stream --format "{{.Name}}\t{{.MemUsage}}\t{{.CPUPerc}}" homarr-go-baseline
```

## Resultados

| Binario | RSS idle | CPU idle | Tamaño imagen |
|---|---|---|---|
| homarr upstream (node+next+redis+nginx) | _pendiente — Docker no disponible en esta máquina; medir en el host_ | — | — |
| homarr-go M0 | **≈ 9 MB** (Windows dev, `tasklist`, placeholder SPA, sin BD) | ~0% | _pendiente_ |

> Nota: Docker no está instalado en la máquina de desarrollo actual. Ejecutar
> estas medidas en el host donde corre (o correrá) Homarr y rellenar la tabla.
