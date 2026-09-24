# Four jobs on one GPU

Runs four quarter-card jobs on a single card at the same time, then shows the
accounting. Needs a running `gmux serve`; on a machine without a GPU, start it
with a fake card:

    gmux serve --fake 1xH100:80G &

Then:

    ./run.sh
