for key in $( set | awk '{FS="="}  /^OS_/ {print $1}' ); do unset $key ; done
export OS_PROJECT_DOMAIN_NAME=
export OS_USER_DOMAIN_NAME=
export OS_PROJECT_NAME=
export OS_USERNAME=
export OS_PASSWORD=
export OS_AUTH_URL=
export OS_IDENTITY_API_VERSION=3
export OS_IMAGE_API_VERSION=2
export OS_PROJECT_DOMAIN_NAME=
export OS_PROJECT_ID=
#export OS_CACERT=
