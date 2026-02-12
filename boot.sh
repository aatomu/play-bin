screen -r play-bin -X stuff "^C\n"
sleep 1s
screen -r play-bin -X kill
screen -U -A -md -S play-bin
screen -r play-bin -X stuff "cd $(dirname $0)/\n"
screen -r play-bin -X stuff "while :; do go run .; done\n"