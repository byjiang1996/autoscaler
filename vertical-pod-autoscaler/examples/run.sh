containsElement () {
  local e match="$1"
  shift
  for e; do [[ "$e" == "$match" ]] && return 0; done
  return 1
}

while true
do
    pods=$(kubectl get pods | cut -d " " -f 1)
    vpas=($(kubectl get vpa | gcut -d " " --output-delimiter=" " -f 1 ))
    for pod in $pods
    do
        if [[ $pod = fc-dep-faro-* ]]; then
            execId=$(echo $pod | cut -d "-" -f 4)
            containsElement "bijiang-azkaban-vpa-$execId" "${vpas[@]}"
            if [[ $? -eq 1 ]]; then
                echo $execId
                cp test.yaml test1.yaml
                sed "s/eid/$execId/g" test1.yaml > test2.yaml
                kubectl apply -f test2.yaml
                sleep 1
            fi
        fi
    done

    sleep 300
done