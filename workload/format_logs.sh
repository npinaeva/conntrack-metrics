cd results
SUM_FILE="summary"
STATS_FILE="stats-tmp"
QUANTILES_FILE="quantiles"
rm -f $SUM_FILE $STATS_FILE $QUANTILES_FILE
CUR_FILE=$SUM_FILE
while IFS= read -r line; do
    if [[ $line == *"This is ApacheBench"* ]]; then
      CUR_FILE=$SUM_FILE
    elif [[ $line == *"starttime	seconds	ctime	dtime	ttime	wait"* ]]; then
      CUR_FILE=$STATS_FILE
      continue
    fi
    echo $line >> $CUR_FILE
done < kube-burner-logs
cat $STATS_FILE | cut -d ' ' -f2- | cat > "stats"
rm $STATS_FILE
TMP_FILE="tmp_file"
for i in {7,9}; do
  cat "stats" | cut -d ' ' -f"$i" | sort -n > $TMP_FILE
  if [[ $i == 7 ]]; then
    echo "Connect time" >> $QUANTILES_FILE
  else
    echo "Total time" >> $QUANTILES_FILE
  fi
  for q in {1,10,50,90,95,99}; do
      total=$(wc -l < $TMP_FILE)
      count=$(((total * q + 1) / 100))
      echo -n "$q percentile: " >> $QUANTILES_FILE
      sed -n ${count}p $TMP_FILE >> $QUANTILES_FILE
  done
  rm -f $TMP_FILE
done

cd ..
gnuplot -p gnuplot_connect_time
gnuplot -p gnuplot_total_time

