<script lang='ts'>

import { Map, type MapMouseEvent } from 'maplibre-gl';
import type { ServiceError } from '@viamrobotics/sdk';
import { notify } from '@viamrobotics/prime';
import { setWaypoint, getWaypoints } from '@/api/navigation';
import { setAsyncInterval } from '@/lib/schedule';
import { useRobotClient, useDisconnect } from '@/hooks/robot-client';
import { waypoints } from '../stores';
import MapMarker from './marker.svelte';

export let map: Map;
export let name: string;

const { robotClient } = useRobotClient();

const handleAddMarker = async (event: MapMouseEvent) => {
  if (event.originalEvent.button > 0) {
    return;
  }

  const { lat, lng } = event.lngLat;
  const temp = { lng, lat, id: crypto.randomUUID() };

  try {
    $waypoints = [...$waypoints, temp];
    await setWaypoint($robotClient, lat, lng, name);
  } catch (error) {
    notify.danger((error as ServiceError).message);
    $waypoints = $waypoints.filter((item) => item.id !== temp.id);
  }
};

const updateWaypoints = async () => {
  try {
    $waypoints = await getWaypoints($robotClient, name);
  } catch (error) {
    notify.danger((error as ServiceError).message);
  }
};

const clearUpdateWaypointInterval = setAsyncInterval(updateWaypoints, 1000);
updateWaypoints();

useDisconnect(() => clearUpdateWaypointInterval());

map.on('click', handleAddMarker);

</script>

{#each $waypoints as waypoint (waypoint.id)}
  <MapMarker scale={0.7} lng={waypoint.lng} lat={waypoint.lat} />
{/each}
