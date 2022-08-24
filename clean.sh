#STOP AND DELETE THE DOCKER CONTAINERS
docker ps -aq | xargs -n 1 docker stop
docker ps -aq | xargs -n 1 docker rm -v
#DELETE THE OLD DOCKER VOLUMES
docker volume prune -f
#DELETE OLD DOCKER NETWORKS
docker network prune -f
#Remove all stopped containers
docker container prune -f
#Remove All Unused Objects
docker system prune -f
docker system prune --volumes -f
#DELETE SCRIPT-CREATED FILES
rm -rf channel-artifacts/*.block channel-artifacts/*.tx crypto-config
#if you get error in future you may also need to remove docker images using the commands given below:
docker rm -f $(docker ps -aq)
docker rmi -f $(docker images -q)

function removeUnwantedImages() {
  DOCKER_IMAGE_IDS=$(docker images | awk '($1 ~ /dev-peer.*/) {print $3}')
  if [ -z "$DOCKER_IMAGE_IDS" -o "$DOCKER_IMAGE_IDS" == " " ]; then
    echo "---- No images available for deletion ----"
  else
    docker rmi -f $DOCKER_IMAGE_IDS
  fi
}

removeUnwantedImages


docker rmi -f $(docker images | awk '($1 ~ /dev-peer.*/) {print $3}')
